package keysource

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// kmsEncryptionContext is the context the data key was wrapped with. KMS binds
// the ciphertext to it, so a blob wrapped for another purpose does not open.
var kmsEncryptionContext = map[string]string{"ragmux:purpose": "secret_key"}

// kmsDecrypt opens the wrapped key with KMS Decrypt, signing the call with
// whatever credentials the AWS chain finds.
func (r *resolver) kmsDecrypt(ctx context.Context, o Options) ([]byte, error) {
	const op = "decrypt"
	blob, err := base64.StdEncoding.DecodeString(o.Wrapped)
	if err != nil || len(blob) == 0 {
		return nil, &Error{Class: ClassMalformed, Op: op,
			Detail: "SECRET_KEY_WRAPPED is not a base64 CiphertextBlob"}
	}
	endpoint, err := r.awsEndpoint("kms", o.AWSRegion)
	if err != nil {
		return nil, err
	}
	creds, err := r.awsCredentials(ctx, o.AWSRegion)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(map[string]any{
		"CiphertextBlob":    blob,
		"EncryptionContext": kmsEncryptionContext,
	})
	if err != nil {
		return nil, &Error{Class: ClassMalformed, Op: op, Detail: "encode the request"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, &Error{Class: ClassMalformed, Op: op, Detail: "the KMS endpoint is not a usable URL"}
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", "TrentService.Decrypt")
	signV4(req, payload, creds, o.AWSRegion, "kms", r.now())

	status, body, err := send(r.apiClient(), req, op)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		var e struct {
			Type string `json:"__type"`
		}
		_ = json.Unmarshal(body, &e)
		class, name := kmsClass(status, e.Type)
		return nil, &Error{Class: class, Op: op, Status: status, Detail: name}
	}
	var out struct {
		Plaintext []byte `json:"Plaintext"`
	}
	if err := json.Unmarshal(body, &out); err != nil || len(out.Plaintext) == 0 {
		return nil, &Error{Class: ClassMalformed, Op: op, Status: status,
			Detail: "the reply has no Plaintext"}
	}
	return out.Plaintext, nil
}

// kmsErrors maps the error types KMS returns to a class. Only a name found
// here is ever repeated in an error message.
var kmsErrors = map[string]Class{
	"UnrecognizedClientException":   ClassAuth,
	"InvalidSignatureException":     ClassAuth,
	"IncompleteSignature":           ClassAuth,
	"MissingAuthenticationToken":    ClassAuth,
	"ExpiredTokenException":         ClassAuth,
	"InvalidClientTokenId":          ClassAuth,
	"AccessDeniedException":         ClassDenied,
	"KMSInternalException":          ClassNetwork,
	"DependencyTimeoutException":    ClassNetwork,
	"ThrottlingException":           ClassNetwork,
	"LimitExceededException":        ClassNetwork,
	"ServiceUnavailableException":   ClassNetwork,
	"InvalidCiphertextException":    ClassMalformed,
	"IncorrectKeyException":         ClassMalformed,
	"InvalidKeyUsageException":      ClassMalformed,
	"DisabledException":             ClassMalformed,
	"KMSInvalidStateException":      ClassMalformed,
	"NotFoundException":             ClassMalformed,
	"ValidationException":           ClassMalformed,
	"InvalidGrantTokenException":    ClassMalformed,
	"KeyUnavailableException":       ClassNetwork,
	"KMSInvalidMacException":        ClassMalformed,
	"InvalidMarkerException":        ClassMalformed,
	"InvalidArnException":           ClassMalformed,
	"UnsupportedOperationException": ClassMalformed,
}

// kmsClass classifies a KMS failure and returns the error name when it is one
// of the known ones. __type may be "Name" or "namespace#Name".
func kmsClass(status int, typ string) (Class, string) {
	if i := strings.LastIndex(typ, "#"); i >= 0 {
		typ = typ[i+1:]
	}
	if c, ok := kmsErrors[typ]; ok {
		return c, typ
	}
	return classByStatus(status), ""
}

// awsEndpoint is the HTTPS endpoint of service in region, or the operator's
// override (AWS_ENDPOINT_URL_<SERVICE>, then AWS_ENDPOINT_URL: VPC endpoints,
// FIPS). An override must be https.
func (r *resolver) awsEndpoint(service, region string) (string, error) {
	for _, name := range []string{"AWS_ENDPOINT_URL_" + strings.ToUpper(service), "AWS_ENDPOINT_URL"} {
		v := strings.TrimSpace(r.getenv(name))
		if v == "" {
			continue
		}
		u, err := url.Parse(v)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return "", &Error{Class: ClassMalformed, Op: "endpoint",
				Detail: name + " must be an https URL"}
		}
		return strings.TrimRight(v, "/") + "/", nil
	}
	suffix := ".amazonaws.com"
	if strings.HasPrefix(region, "cn-") {
		suffix = ".amazonaws.com.cn"
	}
	return "https://" + service + "." + region + suffix + "/", nil
}

// apiClient is the HTTP client for the AWS APIs (TLS, system roots).
func (r *resolver) apiClient() httpDoer {
	if r.client != nil {
		return r.client
	}
	return newClient(nil, true)
}

// metadataClient is the HTTP client for the link-local credential endpoints,
// which are plain HTTP and must not go through a proxy.
func (r *resolver) metadataClient() httpDoer {
	if r.client != nil {
		return r.client
	}
	return newClient(nil, false)
}

// awsCredentials walks the credential chain and returns the first source that
// is configured: static environment keys, web identity (EKS IRSA), container
// credentials (ECS, EKS Pod Identity), then EC2 instance metadata (IMDSv2).
// Shared profiles and SSO are not supported.
//
// A source that is configured but fails is an error: falling through to the
// next one would hide the misconfiguration behind a different identity.
func (r *resolver) awsCredentials(ctx context.Context, region string) (awsCreds, error) {
	if id := strings.TrimSpace(r.getenv("AWS_ACCESS_KEY_ID")); id != "" {
		secret := strings.TrimSpace(r.getenv("AWS_SECRET_ACCESS_KEY"))
		if secret == "" {
			return awsCreds{}, &Error{Class: ClassAuth, Op: "credentials",
				Detail: "AWS_ACCESS_KEY_ID is set without AWS_SECRET_ACCESS_KEY"}
		}
		return awsCreds{AccessKeyID: id, SecretAccessKey: secret,
			SessionToken: strings.TrimSpace(r.getenv("AWS_SESSION_TOKEN"))}, nil
	}
	if file, role := r.getenv("AWS_WEB_IDENTITY_TOKEN_FILE"), r.getenv("AWS_ROLE_ARN"); file != "" && role != "" {
		return r.webIdentityCredentials(ctx, region, file, role)
	}
	if r.getenv("AWS_CONTAINER_CREDENTIALS_FULL_URI") != "" || r.getenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI") != "" {
		return r.containerCredentials(ctx)
	}
	if strings.EqualFold(r.getenv("AWS_EC2_METADATA_DISABLED"), "true") {
		return awsCreds{}, noCredentials(ClassAuth, "instance metadata is disabled by AWS_EC2_METADATA_DISABLED")
	}
	c, err := r.imdsCredentials(ctx)
	if err != nil {
		class := ClassAuth
		if ClassOf(err) == ClassNetwork {
			class = ClassNetwork
		}
		return awsCreds{}, noCredentials(class, "instance metadata failed: "+err.Error())
	}
	return c, nil
}

func noCredentials(class Class, why string) error {
	return &Error{Class: class, Op: "credentials", Detail: "no AWS credentials: set " +
		"AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY, a web identity (AWS_WEB_IDENTITY_TOKEN_FILE + AWS_ROLE_ARN), " +
		"container credentials, or run with an instance role (" + why + ")"}
}

// stsClasses maps STS error codes to a class.
var stsClasses = map[string]Class{
	"InvalidIdentityToken":    ClassAuth,
	"ExpiredTokenException":   ClassAuth,
	"IDPRejectedClaim":        ClassAuth,
	"AccessDenied":            ClassDenied,
	"IDPCommunicationError":   ClassNetwork,
	"Throttling":              ClassNetwork,
	"ServiceUnavailable":      ClassNetwork,
	"MalformedPolicyDocument": ClassMalformed,
	"InvalidParameterValue":   ClassMalformed,
}

// webIdentityCredentials exchanges the projected service-account token for
// temporary credentials with the unsigned STS AssumeRoleWithWebIdentity call.
func (r *resolver) webIdentityCredentials(ctx context.Context, region, tokenFile, roleARN string) (awsCreds, error) {
	const op = "assume role with web identity"
	raw, err := os.ReadFile(filepath.Clean(tokenFile))
	if err != nil {
		return awsCreds{}, &Error{Class: ClassAuth, Op: op,
			Detail: "AWS_WEB_IDENTITY_TOKEN_FILE is not readable: " + transportDetail(err)}
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return awsCreds{}, &Error{Class: ClassAuth, Op: op, Detail: "AWS_WEB_IDENTITY_TOKEN_FILE is empty"}
	}
	endpoint, err := r.awsEndpoint("sts", region)
	if err != nil {
		return awsCreds{}, err
	}
	session := strings.TrimSpace(r.getenv("AWS_ROLE_SESSION_NAME"))
	if session == "" {
		session = "ragmux"
	}
	form := url.Values{
		"Action":           {"AssumeRoleWithWebIdentity"},
		"Version":          {"2011-06-15"},
		"RoleArn":          {roleARN},
		"RoleSessionName":  {session},
		"WebIdentityToken": {token},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return awsCreds{}, &Error{Class: ClassMalformed, Op: op, Detail: "the STS endpoint is not a usable URL"}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	status, body, err := send(r.apiClient(), req, op)
	if err != nil {
		return awsCreds{}, err
	}
	if status != http.StatusOK {
		var e struct {
			Error struct {
				Code string `xml:"Code"`
			} `xml:"Error"`
		}
		_ = xml.Unmarshal(body, &e)
		class, ok := stsClasses[e.Error.Code]
		if !ok {
			class = classByStatus(status)
		}
		name := ""
		if ok {
			name = e.Error.Code
		}
		return awsCreds{}, &Error{Class: class, Op: op, Status: status, Detail: name}
	}
	var out struct {
		Credentials struct {
			AccessKeyID     string `xml:"AccessKeyId"`
			SecretAccessKey string `xml:"SecretAccessKey"`
			SessionToken    string `xml:"SessionToken"`
		} `xml:"AssumeRoleWithWebIdentityResult>Credentials"`
	}
	if err := xml.Unmarshal(body, &out); err != nil {
		return awsCreds{}, &Error{Class: ClassMalformed, Op: op, Status: status, Detail: "the reply is not the expected XML"}
	}
	c := out.Credentials
	if c.AccessKeyID == "" || c.SecretAccessKey == "" {
		return awsCreds{}, &Error{Class: ClassMalformed, Op: op, Status: status, Detail: "the reply has no credentials"}
	}
	return awsCreds{AccessKeyID: c.AccessKeyID, SecretAccessKey: c.SecretAccessKey, SessionToken: c.SessionToken}, nil
}

// containerCredentialsHosts are the plain-HTTP hosts the container credential
// endpoint may be on besides loopback: the ECS and the EKS Pod Identity agents.
var containerCredentialsHosts = map[string]bool{
	"169.254.170.2":  true,
	"169.254.170.23": true,
	"fd00:ec2::23":   true,
}

// containerCredentials reads credentials from the ECS / EKS Pod Identity agent.
func (r *resolver) containerCredentials(ctx context.Context) (awsCreds, error) {
	const op = "container credentials"
	target := ""
	if rel := strings.TrimSpace(r.getenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI")); rel != "" {
		target = "http://169.254.170.2" + rel
	} else {
		target = strings.TrimSpace(r.getenv("AWS_CONTAINER_CREDENTIALS_FULL_URI"))
	}
	u, err := url.Parse(target)
	if err != nil || u.Host == "" {
		return awsCreds{}, &Error{Class: ClassMalformed, Op: op, Detail: "the credentials URI is not a URL"}
	}
	if u.Scheme != "https" {
		host := u.Hostname()
		ip := net.ParseIP(host)
		local := host == "localhost" || (ip != nil && ip.IsLoopback()) || containerCredentialsHosts[host]
		if u.Scheme != "http" || !local {
			return awsCreds{}, &Error{Class: ClassMalformed, Op: op,
				Detail: "the credentials URI must be https, or http on a loopback or agent address"}
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return awsCreds{}, &Error{Class: ClassMalformed, Op: op, Detail: "the credentials URI is not usable"}
	}
	auth := strings.TrimSpace(r.getenv("AWS_CONTAINER_AUTHORIZATION_TOKEN"))
	if f := r.getenv("AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE"); f != "" {
		raw, err := os.ReadFile(filepath.Clean(f))
		if err != nil {
			return awsCreds{}, &Error{Class: ClassAuth, Op: op,
				Detail: "AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE is not readable: " + transportDetail(err)}
		}
		auth = strings.TrimSpace(string(raw))
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	status, body, err := send(r.metadataClient(), req, op)
	if err != nil {
		return awsCreds{}, err
	}
	if status != http.StatusOK {
		return awsCreds{}, &Error{Class: classByStatus(status), Op: op, Status: status}
	}
	return parseCredentialsJSON(body, op, status)
}

// parseCredentialsJSON reads the {AccessKeyId, SecretAccessKey, Token} shape
// both the container agents and IMDS return.
func parseCredentialsJSON(body []byte, op string, status int) (awsCreds, error) {
	var c struct {
		AccessKeyID     string `json:"AccessKeyId"`
		SecretAccessKey string `json:"SecretAccessKey"`
		Token           string `json:"Token"`
	}
	if err := json.Unmarshal(body, &c); err != nil || c.AccessKeyID == "" || c.SecretAccessKey == "" {
		return awsCreds{}, &Error{Class: ClassMalformed, Op: op, Status: status,
			Detail: "the reply has no credentials"}
	}
	return awsCreds{AccessKeyID: c.AccessKeyID, SecretAccessKey: c.SecretAccessKey, SessionToken: c.Token}, nil
}

var imdsRolePattern = regexp.MustCompile(`^[A-Za-z0-9+=,.@_-]{1,128}$`)

// imdsCredentials reads the instance role's credentials through IMDSv2: a
// session token first, then the role name, then the credentials.
func (r *resolver) imdsCredentials(ctx context.Context) (awsCreds, error) {
	const op = "instance metadata"
	base := strings.TrimRight(strings.TrimSpace(r.getenv("AWS_EC2_METADATA_SERVICE_ENDPOINT")), "/")
	if base == "" {
		base = "http://169.254.169.254"
	}
	client := r.metadataClient()
	get := func(method, path string, hdr map[string]string) (int, []byte, error) {
		req, err := http.NewRequestWithContext(ctx, method, base+path, nil)
		if err != nil {
			return 0, nil, &Error{Class: ClassMalformed, Op: op, Detail: "AWS_EC2_METADATA_SERVICE_ENDPOINT is not a usable URL"}
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		return send(client, req, op)
	}
	status, body, err := get(http.MethodPut, "/latest/api/token", map[string]string{"X-aws-ec2-metadata-token-ttl-seconds": "60"})
	if err != nil {
		return awsCreds{}, err
	}
	if status != http.StatusOK {
		return awsCreds{}, &Error{Class: imdsClass(status), Op: op, Status: status, Detail: "no IMDSv2 session token"}
	}
	hdr := map[string]string{"X-aws-ec2-metadata-token": strings.TrimSpace(string(body))}
	status, body, err = get(http.MethodGet, "/latest/meta-data/iam/security-credentials/", hdr)
	if err != nil {
		return awsCreds{}, err
	}
	if status != http.StatusOK {
		return awsCreds{}, &Error{Class: imdsClass(status), Op: op, Status: status, Detail: "no instance role"}
	}
	role, _, _ := strings.Cut(strings.TrimSpace(string(body)), "\n")
	role = strings.TrimSpace(role)
	if !imdsRolePattern.MatchString(role) {
		return awsCreds{}, &Error{Class: ClassMalformed, Op: op, Status: status, Detail: "the role name is not valid"}
	}
	status, body, err = get(http.MethodGet, "/latest/meta-data/iam/security-credentials/"+role, hdr)
	if err != nil {
		return awsCreds{}, err
	}
	if status != http.StatusOK {
		return awsCreds{}, &Error{Class: imdsClass(status), Op: op, Status: status}
	}
	return parseCredentialsJSON(body, op, status)
}

// imdsClass: a refusal from IMDS means this host has no usable role, which is
// an authentication problem, not a transient one.
func imdsClass(status int) Class {
	if status >= 500 || status == http.StatusTooManyRequests {
		return ClassNetwork
	}
	return ClassAuth
}
