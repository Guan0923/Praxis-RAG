package payment

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"unicode"
)

// SignTypeRSA2 is the only signature algorithm this package asks Alipay to use.
// The form value sign_type is never trusted to select another algorithm.
const SignTypeRSA2 = "RSA2"

// Errors returned while handling keys, signatures and gateway responses.
var (
	// ErrInvalidKey reports an unparsable or missing RSA key.
	ErrInvalidKey = errors.New("支付宝密钥格式不正确")
	// ErrInvalidSignature reports a missing or mismatching RSA2 signature.
	ErrInvalidSignature = errors.New("支付宝签名校验失败")
)

// SignValues signs the non-empty values of one parameter map with the
// application private key and returns the standard base64 signature.
//
// The signed text follows the Alipay rule: drop sign and sign_type, drop empty
// values, sort the remaining keys lexicographically and join them as
// "key=value" with "&". Values are used verbatim and are never URL encoded.
func SignValues(values map[string]string, privateKeyPEM string) (string, error) {
	key, err := parsePrivateKey(privateKeyPEM)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(signContent(values)))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", errors.New("支付宝签名计算失败")
	}
	return base64.StdEncoding.EncodeToString(signature), nil
}

// VerifyValues checks the RSA2 signature carried in values["sign"].
//
// It returns a Chinese security error on failure and never echoes the signature
// or the signed content.
func VerifyValues(values map[string]string, publicKeyPEM string) error {
	signature := strings.TrimSpace(values["sign"])
	if signature == "" {
		return fmt.Errorf("%w：缺少签名", ErrInvalidSignature)
	}
	return verifySignature(signContent(values), signature, publicKeyPEM)
}

// SignFormValues signs one url.Values form (for example a browser form or an
// Alipay notification) and returns the standard base64 signature.
func SignFormValues(values url.Values, privateKeyPEM string) (string, error) {
	flat, err := flattenForm(values)
	if err != nil {
		return "", err
	}
	return SignValues(flat, privateKeyPEM)
}

// VerifyFormValues checks the RSA2 signature of one url.Values form. Repeated
// parameters are rejected instead of silently picking the first value.
func VerifyFormValues(values url.Values, publicKeyPEM string) error {
	flat, err := flattenForm(values)
	if err != nil {
		return err
	}
	return VerifyValues(flat, publicKeyPEM)
}

// signContent builds the canonical text described by SignValues.
func signContent(values map[string]string) string {
	keys := make([]string, 0, len(values))
	for key, value := range values {
		if key == "sign" || key == "sign_type" || value == "" {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var builder strings.Builder
	for index, key := range keys {
		if index > 0 {
			builder.WriteByte('&')
		}
		builder.WriteString(key)
		builder.WriteByte('=')
		builder.WriteString(values[key])
	}
	return builder.String()
}

// flattenForm converts one url.Values into a plain map. Parameters that appear
// more than once are rejected because Alipay never sends repeated fields and
// accepting them would let an attacker choose which copy is verified.
func flattenForm(values url.Values) (map[string]string, error) {
	flat := make(map[string]string, len(values))
	for key, list := range values {
		switch len(list) {
		case 0:
			flat[key] = ""
		case 1:
			flat[key] = list[0]
		default:
			return nil, fmt.Errorf("%w：参数 %s 重复", ErrInvalidRequest, key)
		}
	}
	return flat, nil
}

// verifySignature checks one RSA PKCS#1 v1.5 SHA-256 signature.
func verifySignature(content, signature, publicKeyPEM string) error {
	key, err := parsePublicKey(publicKeyPEM)
	if err != nil {
		return err
	}
	raw, err := decodeSignature(signature)
	if err != nil {
		return fmt.Errorf("%w：签名编码无效", ErrInvalidSignature)
	}
	digest := sha256.Sum256([]byte(content))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], raw); err != nil {
		return fmt.Errorf("%w：签名不一致", ErrInvalidSignature)
	}
	return nil
}

// decodeSignature decodes the standard base64 signature. Alipay notifications
// are form encoded; a "+" that travelled unescaped arrives as a space, and a
// space is never part of base64, so restoring it is safe.
func decodeSignature(signature string) ([]byte, error) {
	normalized := strings.ReplaceAll(strings.TrimSpace(signature), " ", "+")
	raw, err := base64.StdEncoding.DecodeString(normalized)
	if err == nil {
		return raw, nil
	}
	return base64.RawStdEncoding.DecodeString(normalized)
}

// verifyGatewayResponse extracts the exact JSON object bytes of nodeName from a
// raw gateway response, verifies the RSA2 signature over those bytes and
// returns the object. The bytes are never re-marshalled: Alipay signs the
// verbatim JSON node, and re-encoding could change whitespace or key order.
func verifyGatewayResponse(body []byte, nodeName, publicKeyPEM string) (json.RawMessage, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("%w：支付宝返回内容无法解析", ErrInvalidResponse)
	}
	node, ok := envelope[nodeName]
	if !ok || len(node) == 0 {
		return nil, fmt.Errorf("%w：支付宝返回内容缺少 %s", ErrInvalidResponse, nodeName)
	}
	rawSignature, ok := envelope["sign"]
	if !ok {
		return nil, fmt.Errorf("%w：支付宝响应缺少签名", ErrInvalidSignature)
	}
	var signature string
	if err := json.Unmarshal(rawSignature, &signature); err != nil || strings.TrimSpace(signature) == "" {
		return nil, fmt.Errorf("%w：支付宝响应缺少签名", ErrInvalidSignature)
	}
	if err := verifySignature(string(node), signature, publicKeyPEM); err != nil {
		return nil, fmt.Errorf("支付宝响应验签失败：%w", err)
	}
	return node, nil
}

// normalizeKeyMaterial trims one key value and expands the literal `\n`
// sequences that single-line environment variables use.
func normalizeKeyMaterial(raw string) string {
	material := strings.TrimSpace(raw)
	material = strings.ReplaceAll(material, `\n`, "\n")
	material = strings.ReplaceAll(material, `\r`, "\r")
	return strings.TrimSpace(material)
}

// parsePrivateKey parses the application private key from PKCS#1 PEM, PKCS#8
// PEM or bare base64 DER.
func parsePrivateKey(raw string) (*rsa.PrivateKey, error) {
	material := normalizeKeyMaterial(raw)
	if material == "" {
		return nil, fmt.Errorf("%w：未配置支付宝应用私钥", ErrInvalidKey)
	}
	if strings.Contains(material, "-----BEGIN") {
		return parsePrivateKeyPEM(material)
	}
	der, err := decodeBase64DER(material)
	if err != nil {
		return nil, invalidPrivateKeyError()
	}
	key, err := parsePrivateKeyDER(der)
	if err != nil {
		return nil, invalidPrivateKeyError()
	}
	return key, nil
}

// parsePrivateKeyPEM walks every PEM block of the material and returns the first
// RSA private key it can parse.
func parsePrivateKeyPEM(material string) (*rsa.PrivateKey, error) {
	rest := []byte(material)
	for {
		block, remaining := pem.Decode(rest)
		if block == nil {
			break
		}
		rest = remaining
		switch block.Type {
		case "RSA PRIVATE KEY":
			key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
			if err == nil {
				return key, nil
			}
		case "PRIVATE KEY":
			parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
			if err == nil {
				if key, ok := parsed.(*rsa.PrivateKey); ok {
					return key, nil
				}
			}
		case "ENCRYPTED PRIVATE KEY":
			return nil, fmt.Errorf("%w：不支持加密的支付宝应用私钥", ErrInvalidKey)
		}
	}
	return nil, invalidPrivateKeyError()
}

// parsePrivateKeyDER parses one PKCS#8 or PKCS#1 DER private key.
func parsePrivateKeyDER(der []byte) (*rsa.PrivateKey, error) {
	if parsed, err := x509.ParsePKCS8PrivateKey(der); err == nil {
		if key, ok := parsed.(*rsa.PrivateKey); ok {
			return key, nil
		}
	}
	key, err := x509.ParsePKCS1PrivateKey(der)
	if err != nil {
		return nil, err
	}
	return key, nil
}

// parsePublicKey parses the Alipay public key from PKIX PEM, X.509 certificate
// PEM or bare base64 DER.
func parsePublicKey(raw string) (*rsa.PublicKey, error) {
	material := normalizeKeyMaterial(raw)
	if material == "" {
		return nil, fmt.Errorf("%w：未配置支付宝公钥", ErrInvalidKey)
	}
	if strings.Contains(material, "-----BEGIN") {
		return parsePublicKeyPEM(material)
	}
	der, err := decodeBase64DER(material)
	if err != nil {
		return nil, invalidPublicKeyError()
	}
	key, err := parsePublicKeyDER(der)
	if err != nil {
		return nil, invalidPublicKeyError()
	}
	return key, nil
}

// parsePublicKeyPEM walks every PEM block of the material and returns the first
// RSA public key or certificate public key it can parse.
func parsePublicKeyPEM(material string) (*rsa.PublicKey, error) {
	rest := []byte(material)
	for {
		block, remaining := pem.Decode(rest)
		if block == nil {
			break
		}
		rest = remaining
		switch block.Type {
		case "PUBLIC KEY":
			key, err := parsePKIXPublicKey(block.Bytes)
			if err == nil {
				return key, nil
			}
		case "RSA PUBLIC KEY":
			key, err := x509.ParsePKCS1PublicKey(block.Bytes)
			if err == nil {
				return key, nil
			}
		case "CERTIFICATE":
			certificate, err := x509.ParseCertificate(block.Bytes)
			if err == nil {
				if key, ok := certificate.PublicKey.(*rsa.PublicKey); ok {
					return key, nil
				}
			}
		}
	}
	return nil, invalidPublicKeyError()
}

// parsePublicKeyDER parses one PKIX, certificate or PKCS#1 DER public key.
func parsePublicKeyDER(der []byte) (*rsa.PublicKey, error) {
	if key, err := parsePKIXPublicKey(der); err == nil {
		return key, nil
	}
	if certificate, err := x509.ParseCertificate(der); err == nil {
		if key, ok := certificate.PublicKey.(*rsa.PublicKey); ok {
			return key, nil
		}
	}
	return x509.ParsePKCS1PublicKey(der)
}

// parsePKIXPublicKey parses one PKIX DER public key and requires RSA.
func parsePKIXPublicKey(der []byte) (*rsa.PublicKey, error) {
	parsed, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, err
	}
	key, ok := parsed.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("public key is not RSA")
	}
	return key, nil
}

// decodeBase64DER decodes one bare base64 DER value, ignoring any whitespace
// introduced by line wrapping.
func decodeBase64DER(material string) ([]byte, error) {
	compact := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, material)
	if der, err := base64.StdEncoding.DecodeString(compact); err == nil {
		return der, nil
	}
	return base64.RawStdEncoding.DecodeString(compact)
}

// invalidPrivateKeyError is the safe error for an unusable private key.
func invalidPrivateKeyError() error {
	return fmt.Errorf("%w：支付宝应用私钥格式不正确（支持 PKCS#1/PKCS#8 PEM 或 base64 DER）", ErrInvalidKey)
}

// invalidPublicKeyError is the safe error for an unusable public key.
func invalidPublicKeyError() error {
	return fmt.Errorf("%w：支付宝公钥格式不正确（支持 PKIX PEM、证书 PEM 或 base64 DER）", ErrInvalidKey)
}
