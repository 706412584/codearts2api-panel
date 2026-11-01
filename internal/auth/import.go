package auth

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ParseImport accepts stored accounts, STS responses and Python AK/SK exports.
// It parses and validates only; the caller chooses when to persist the result.
func ParseImport(raw []byte) ([]*Auth, error) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("invalid credential JSON: %w", err)
	}
	if object, ok := value.(map[string]any); ok {
		if accounts, ok := object["accounts"].([]any); ok {
			value = accounts
		}
	}
	items, ok := value.([]any)
	if !ok {
		items = []any{value}
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("no credentials supplied")
	}
	if len(items) > 100 {
		return nil, fmt.Errorf("at most 100 accounts per import")
	}
	out := make([]*Auth, 0, len(items))
	seen := map[string]bool{}
	for i, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("account %d must be an object", i+1)
		}
		encoded, _ := json.Marshal(object)
		var a Auth
		if err := json.Unmarshal(encoded, &a); err != nil {
			return nil, fmt.Errorf("account %d has invalid fields", i+1)
		}
		read := func(m map[string]any, keys ...string) string {
			for _, key := range keys {
				if text, ok := m[key].(string); ok && strings.TrimSpace(text) != "" {
					return strings.TrimSpace(text)
				}
			}
			return ""
		}
		if a.AccessKeyID == "" {
			a.AccessKeyID = read(object, "ak", "AK", "CODEARTS_AK", "accessKeyId")
		}
		if a.SecretAccessKey == "" {
			a.SecretAccessKey = read(object, "sk", "SK", "CODEARTS_SK", "secretAccessKey")
		}
		if a.CloudDragonTok == "" {
			a.CloudDragonTok = read(object, "security_token", "securityToken", "securitytoken")
		}
		if a.Expiration == "" {
			a.Expiration = read(object, "expires_at", "expiresAt")
		}
		for _, key := range []string{"credentials", "credential", "result"} {
			if nested, ok := object[key].(map[string]any); ok {
				if a.AccessKeyID == "" {
					a.AccessKeyID = read(nested, "access_key_id", "accessKeyId", "access")
				}
				if a.SecretAccessKey == "" {
					a.SecretAccessKey = read(nested, "secret_access_key", "secretAccessKey", "secret")
				}
				if a.CloudDragonTok == "" {
					a.CloudDragonTok = read(nested, "security_token", "securityToken", "securitytoken")
				}
				if a.Expiration == "" {
					a.Expiration = read(nested, "expiration", "expires_at", "expiresAt")
				}
			}
		}
		if a.DPoPPrivateKey == nil {
			if jwk, ok := object["dpop_private_key_jwk"].(map[string]any); ok {
				a.DPoPPrivateKey = map[string]string{}
				for key, val := range jwk {
					if text, ok := val.(string); ok {
						a.DPoPPrivateKey[key] = text
					}
				}
			}
		}
		if err := validate(&a, true); err != nil {
			return nil, fmt.Errorf("account %d: %w", i+1, err)
		}
		if a.UserID == "" {
			sum := sha256.Sum256([]byte(a.AccessKeyID))
			a.UserID = fmt.Sprintf("ak-%x", sum[:12])
		}
		if a.UserName == "" {
			a.UserName = a.UserID
		}
		if a.ClientID == "" && a.RefreshToken != "" {
			a.ClientID = "codearts-agent"
		}
		if seen[a.UserID] {
			return nil, fmt.Errorf("duplicate account id in import")
		}
		seen[a.UserID] = true
		a.UpdatedAt = time.Now().Unix()
		out = append(out, &a)
	}
	return out, nil
}

func validate(a *Auth, importing bool) error {
	a.AccessKeyID, a.SecretAccessKey = strings.TrimSpace(a.AccessKeyID), strings.TrimSpace(a.SecretAccessKey)
	if a.AccessKeyID == "" || a.SecretAccessKey == "" {
		return fmt.Errorf("complete AK and SK are required")
	}
	for _, value := range []string{a.AccessKeyID, a.SecretAccessKey, a.CloudDragonTok} {
		if strings.ContainsAny(value, "\r\n\x00") {
			return fmt.Errorf("credentials contain invalid control characters")
		}
	}
	temporary := a.CloudDragonTok != "" || a.Expiration != "" || a.RefreshToken != ""
	if temporary {
		if a.CloudDragonTok == "" {
			return fmt.Errorf("temporary credentials require a security token")
		}
		expires, err := time.Parse(time.RFC3339, a.Expiration)
		if err != nil {
			return fmt.Errorf("temporary credentials require an RFC3339 expiration")
		}
		if importing && !expires.After(time.Now()) && !a.Refreshable() {
			return fmt.Errorf("temporary credentials expired and cannot be refreshed")
		}
	}
	return nil
}
