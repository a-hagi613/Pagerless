package base

// Auth is a reference to credentials. It never carries a value.
type Auth struct {
	// SecretName names the Secret that holds the values. Empty means the
	// credentials Secret mounted at the configured credentials path.
	SecretName string `yaml:"secretName,omitempty"`
	// TokenKey names the key that holds a bearer token.
	TokenKey string `yaml:"tokenKey,omitempty"`
	// UsernameKey names the key that holds a basic-auth username.
	UsernameKey string `yaml:"usernameKey,omitempty"`
	// PasswordKey names the key that holds a basic-auth password.
	PasswordKey string `yaml:"passwordKey,omitempty"`
	// APIKeyKey names the key that holds an API key.
	APIKeyKey string `yaml:"apiKeyKey,omitempty"`
	// AppKeyKey names the key that holds an application key.
	AppKeyKey string `yaml:"appKeyKey,omitempty"`
	// AccessKeyIDKey names the key that holds an AWS access key id.
	AccessKeyIDKey string `yaml:"accessKeyIDKey,omitempty"`
	// SecretAccessKeyKey names the key that holds an AWS secret access key.
	SecretAccessKeyKey string `yaml:"secretAccessKeyKey,omitempty"`
	// Mode is irsa or static for AWS, and ignored elsewhere.
	Mode string `yaml:"mode,omitempty"`
}
