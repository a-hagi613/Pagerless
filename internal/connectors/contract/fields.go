package contract

// LogFields returns the documented template fields for one log query. Every
// field is supplied so a rendered query can never silently drop a namespace.
func LogFields(q Query, clusterName, environment string) map[string]string {
	return map[string]string{
		"Namespace":    q.Namespace,
		"Kind":         string(q.Subject.Kind),
		"Workload":     q.Subject.Name,
		"Pod":          q.Pod,
		"PodPrefix":    q.Subject.Name + "-",
		"Container":    q.Container,
		"ClusterName":  clusterName,
		"Environment":  environment,
		"SinceRFC3339": q.Since.UTC().Format("2006-01-02T15:04:05Z07:00"),
		"UntilRFC3339": q.Until.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
}

// MetricFields returns the documented template fields for one metrics query.
func MetricFields(q MetricQuery, clusterName, environment string) map[string]string {
	return map[string]string{
		"Namespace":    q.Namespace,
		"Kind":         string(q.Subject.Kind),
		"Workload":     q.Subject.Name,
		"Pod":          "",
		"PodPrefix":    q.Subject.Name + "-",
		"Container":    "",
		"ClusterName":  clusterName,
		"Environment":  environment,
		"SinceRFC3339": q.Since.UTC().Format("2006-01-02T15:04:05Z07:00"),
		"UntilRFC3339": q.Until.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
}
