package webhooks

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/a-hagi613/pagerless/api/v1alpha1"
	"github.com/a-hagi613/pagerless/internal/subject"
)

// snsNotificationType is the SNS message type that carries an alert.
const snsNotificationType = "Notification"

// Ignore is an alert that the receiver deliberately ignores. It is reported to
// the sender as a per-alert reason with a 202 response, never as an error.
type Ignore struct {
	// Reason is a short, stable explanation.
	Reason string
}

// Error implements error.
func (e *Ignore) Error() string { return e.Reason }

// snsEnvelope is the SNS HTTP notification envelope.
type snsEnvelope struct {
	Type             string `json:"Type"`
	MessageID        string `json:"MessageId"`
	TopicARN         string `json:"TopicArn"`
	Subject          string `json:"Subject"`
	Message          string `json:"Message"`
	Timestamp        string `json:"Timestamp"`
	SignatureVersion string `json:"SignatureVersion"`
	Signature        string `json:"Signature"`
	SigningCertURL   string `json:"SigningCertURL"`
	SubscribeURL     string `json:"SubscribeURL"`
	Token            string `json:"Token"`
}

// cloudWatchAlarm is the CloudWatch alarm message inside an SNS envelope.
type cloudWatchAlarm struct {
	AlarmName        string `json:"AlarmName"`
	AlarmDescription string `json:"AlarmDescription"`
	AWSAccountID     string `json:"AWSAccountId"`
	NewStateValue    string `json:"NewStateValue"`
	NewStateReason   string `json:"NewStateReason"`
	StateChangeTime  string `json:"StateChangeTime"`
	Region           string `json:"Region"`
	AlarmARN         string `json:"AlarmArn"`
	Trigger          struct {
		MetricName string `json:"MetricName"`
		Namespace  string `json:"Namespace"`
		Dimensions []struct {
			Name  string `json:"name"`
			Value string `json:"Value"`
		} `json:"Dimensions"`
	} `json:"Trigger"`
}

// snsCertificateCache keeps the signing certificate of each URL, so a retry
// storm does not fetch it for every delivery.
type snsCertificateCache struct {
	mutex    sync.Mutex
	messages map[string]*rsa.PublicKey
}

// certificate returns the cached public key of a certificate URL.
func (c *snsCertificateCache) certificate(
	ctx context.Context, fetch func(context.Context, string) ([]byte, error), certURL string,
) (*rsa.PublicKey, error) {
	c.mutex.Lock()
	if key, ok := c.messages[certURL]; ok {
		c.mutex.Unlock()
		return key, nil
	}
	c.mutex.Unlock()
	body, err := fetch(ctx, certURL)
	if err != nil {
		return nil, fmt.Errorf("fetch the SNS signing certificate: %w", err)
	}
	block, _ := pem.Decode(body)
	if block == nil {
		return nil, errors.New("the SNS signing certificate is not PEM encoded")
	}
	parsed, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse the SNS signing certificate: %w", err)
	}
	key, ok := parsed.PublicKey.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("the SNS signing certificate does not hold an RSA key")
	}
	c.mutex.Lock()
	c.messages[certURL] = key
	c.mutex.Unlock()
	return key, nil
}

// decodeSNS decodes an SNS or CloudWatch delivery.
func decodeSNS(request decodeRequest) ([]Alert, int, error) {
	var envelope snsEnvelope
	if err := json.Unmarshal(request.body, &envelope); err != nil {
		return nil, 0, BadRequest("malformed SNS payload: %v", err)
	}
	if envelope.Type == "" {
		return nil, 0, BadRequest("the SNS payload has no Type")
	}
	if len(request.auth.snsTopics) > 0 && !slices.Contains(request.auth.snsTopics, envelope.TopicARN) {
		return nil, 0, Forbidden("the SNS topic is not allowed")
	}
	switch envelope.Type {
	case "SubscriptionConfirmation", "UnsubscribeConfirmation":
		// The receiver never confirms a subscription. Confirmation is a human
		// decision, so an unknown subscription cannot be attached by a sender.
		return nil, 0, &Ignore{Reason: "subscription-confirmation-not-accepted"}
	case snsNotificationType:
	default:
		return nil, 0, BadRequest("unsupported SNS message type %q", envelope.Type)
	}
	if request.auth.verifySNS {
		if err := verifySNSSignature(request, envelope); err != nil {
			return nil, 0, err
		}
	}
	var alarm cloudWatchAlarm
	if err := json.Unmarshal([]byte(envelope.Message), &alarm); err != nil {
		return nil, 0, BadRequest("malformed CloudWatch alarm in the SNS message: %v", err)
	}
	if alarm.AlarmName == "" {
		return nil, 0, BadRequest("the CloudWatch alarm has no AlarmName")
	}
	labels := cloudWatchLabels(&alarm)
	severity := severityOf(labels["severity"])
	if labels["severity"] == "" {
		if strings.EqualFold(alarm.NewStateValue, "ALARM") {
			severity = v1alpha1.SeverityWarning
		} else {
			severity = v1alpha1.SeverityInfo
		}
	}
	alert := Alert{
		Source: SourceSNS, Sender: string(SourceSNS),
		Status: statusOf(alarm.NewStateValue, "firing"), Name: alarm.AlarmName,
		Fingerprint: firstNonEmpty(alarm.AlarmARN, envelope.MessageID),
		Severity:    severity, Labels: labels,
		Annotations: map[string]string{
			"summary":    firstNonEmpty(alarm.NewStateReason, alarm.AlarmDescription),
			"awsRegion":  alarm.Region,
			"awsAccount": alarm.AWSAccountID,
		},
		Message: firstNonEmpty(alarm.NewStateReason, alarm.AlarmDescription),
	}
	alert.StartsAt = parseTime(alarm.StateChangeTime)
	if alert.Fingerprint == "" {
		alert.Fingerprint = stableFingerprint(alert)
	}
	capped, skipped := limitAlerts([]Alert{alert}, request.limit())
	return capped, skipped, nil
}

// cloudWatchLabels maps the alarm's dimensions to labels the shared resolver
// understands. A dimension whose name starts with pagerless.dev/ is copied
// verbatim, so an operator can point an alarm at a subject explicitly.
func cloudWatchLabels(alarm *cloudWatchAlarm) map[string]string {
	labels := map[string]string{"alertname": alarm.AlarmName}
	if alarm.Trigger.Namespace != "" {
		labels["awsNamespace"] = alarm.Trigger.Namespace
	}
	if alarm.Trigger.MetricName != "" {
		labels["metricName"] = alarm.Trigger.MetricName
	}
	for _, dimension := range alarm.Trigger.Dimensions {
		key := strings.TrimSpace(dimension.Name)
		value := strings.TrimSpace(dimension.Value)
		if key == "" || value == "" {
			continue
		}
		if strings.HasPrefix(key, "pagerless.dev/") {
			labels[key] = value
			continue
		}
		switch strings.ToLower(key) {
		case "namespace", "kube_namespace":
			labels[subject.LabelNamespace] = value
		case "deployment", "kube_deployment":
			labels[subject.LabelDeployment] = value
		case "statefulset", "kube_statefulset":
			labels[subject.LabelStatefulSet] = value
		case "daemonset", "kube_daemonset":
			labels[subject.LabelDaemonSet] = value
		case "job", "kube_job":
			labels[subject.LabelJob] = value
		case "pod", "kube_pod":
			labels[subject.LabelPod] = value
		case "severity":
			labels["severity"] = value
		default:
			labels[key] = value
		}
	}
	return labels
}

// verifySNSSignature verifies the SNS signature of an envelope. The certificate
// must come from an SNS host over HTTPS, so a hostile sender cannot point the
// receiver at an arbitrary key.
func verifySNSSignature(request decodeRequest, envelope snsEnvelope) error {
	if envelope.Signature == "" || envelope.SigningCertURL == "" {
		return Unauthorized("unauthorized")
	}
	if err := validateCertificateURL(envelope.SigningCertURL); err != nil {
		return Unauthorized("unauthorized")
	}
	cache := certificateCache()
	key, err := cache.certificate(request.ctx, fetchCertificate, envelope.SigningCertURL)
	if err != nil {
		return Unauthorized("unauthorized")
	}
	payload := []byte(snsStringToSign(envelope))
	signature, err := base64.StdEncoding.DecodeString(envelope.Signature)
	if err != nil {
		return Unauthorized("unauthorized")
	}
	var hash crypto.Hash
	var digest []byte
	switch envelope.SignatureVersion {
	case "1":
		sum := sha1.Sum(payload)
		hash, digest = crypto.SHA1, sum[:]
	case "2", "":
		sum := sha256.Sum256(payload)
		hash, digest = crypto.SHA256, sum[:]
	default:
		return Unauthorized("unauthorized")
	}
	if err := rsa.VerifyPKCS1v15(key, hash, digest, signature); err != nil {
		return Unauthorized("unauthorized")
	}
	return nil
}

// snsStringToSign builds the canonical string AWS signs.
func snsStringToSign(envelope snsEnvelope) string {
	if envelope.Type == "Notification" {
		return "Message\n" + envelope.Message + "\n" +
			"MessageId\n" + envelope.MessageID + "\n" +
			"Timestamp\n" + envelope.Timestamp + "\n" +
			"TopicArn\n" + envelope.TopicARN + "\n" +
			"Type\n" + envelope.Type + "\n"
	}
	return "Message\n" + envelope.Message + "\n" +
		"MessageId\n" + envelope.MessageID + "\n" +
		"SubscribeURL\n" + envelope.SubscribeURL + "\n" +
		"Timestamp\n" + envelope.Timestamp + "\n" +
		"Token\n" + envelope.Token + "\n" +
		"TopicArn\n" + envelope.TopicARN + "\n" +
		"Type\n" + envelope.Type + "\n"
}

// validateCertificateURL checks that a certificate URL is an SNS host.
func validateCertificateURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if parsed.Scheme != "https" {
		return errors.New("the certificate URL must use https")
	}
	host := strings.ToLower(parsed.Hostname())
	if host != "sns.amazonaws.com" && !strings.HasSuffix(host, ".amazonaws.com") {
		return errors.New("the certificate URL is not an SNS host")
	}
	if !strings.HasPrefix(host, "sns.") && host != "sns.amazonaws.com" {
		return errors.New("the certificate URL is not an SNS host")
	}
	if !strings.HasSuffix(strings.ToLower(parsed.Path), ".pem") {
		return errors.New("the certificate URL is not a certificate")
	}
	return nil
}

// fetchCertificate downloads one signing certificate with a bounded timeout.
func fetchCertificate(ctx context.Context, raw string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("certificate URL returned HTTP %d", response.StatusCode)
	}
	return io.ReadAll(io.LimitReader(response.Body, 64<<10))
}

// certificateCache returns the process certificate cache.
func certificateCache() *snsCertificateCache {
	return processCertificates
}

// processCertificates is the process-wide certificate cache.
var processCertificates = &snsCertificateCache{messages: make(map[string]*rsa.PublicKey)}

// setCertificateForTest installs a public key for a URL. It is called only by
// tests, so a verified signature never needs a network.
func setCertificateForTest(certURL string, key *rsa.PublicKey) {
	processCertificates.mutex.Lock()
	defer processCertificates.mutex.Unlock()
	if processCertificates.messages == nil {
		processCertificates.messages = make(map[string]*rsa.PublicKey)
	}
	processCertificates.messages[certURL] = key
}
