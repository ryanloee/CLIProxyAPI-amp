package codebuddy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/misc"
)

// CodebuddyTokenStorage stores OAuth token information for Codebuddy API authentication.
type CodebuddyTokenStorage struct {
	AccessToken  string         `json:"access_token"`
	RefreshToken string         `json:"refresh_token,omitempty"`
	TokenType    string         `json:"token_type,omitempty"`
	ExpiresAt    int64          `json:"expires_at,omitempty"`
	Domain       string         `json:"domain,omitempty"`
	Expired      string         `json:"expired,omitempty"`
	Type         string         `json:"type"`
	Email        string         `json:"email,omitempty"`
	UID          string         `json:"uid,omitempty"`
	Nickname     string         `json:"nickname,omitempty"`
	Metadata     map[string]any `json:"-"`
}

// SetMetadata allows external callers to inject metadata into the storage before saving.
func (ts *CodebuddyTokenStorage) SetMetadata(meta map[string]any) {
	ts.Metadata = meta
}

// CodebuddyTokenData holds the raw OAuth token response from Codebuddy.
type CodebuddyTokenData struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresAt    int64  `json:"expires_at"`
	Domain       string `json:"domain"`
}

// CodebuddyAccountInfo holds user account information from Codebuddy.
type CodebuddyAccountInfo struct {
	UID            string `json:"uid"`
	Email          string `json:"email"`
	Nickname       string `json:"nickname"`
	EnterpriseID   string `json:"enterpriseId"`
	EnterpriseName string `json:"enterpriseName"`
}

// CodebuddyAuthBundle bundles authentication data for storage.
type CodebuddyAuthBundle struct {
	TokenData   *CodebuddyTokenData
	AccountInfo *CodebuddyAccountInfo
}

// DeviceCodeResponse represents Codebuddy's auth state response.
type DeviceCodeResponse struct {
	LoginID                 string
	State                   string
	VerificationURI         string
	VerificationURIComplete string
	ExpiresIn               int
	Interval                int
}

// SaveTokenToFile serializes the Codebuddy token storage to a JSON file.
func (ts *CodebuddyTokenStorage) SaveTokenToFile(authFilePath string) error {
	misc.LogSavingCredentials(authFilePath)
	ts.Type = "codebuddy"

	if err := os.MkdirAll(filepath.Dir(authFilePath), 0700); err != nil {
		return fmt.Errorf("failed to create directory: %v", err)
	}

	f, err := os.Create(authFilePath)
	if err != nil {
		return fmt.Errorf("failed to create token file: %w", err)
	}
	defer func() {
		_ = f.Close()
	}()

	data, errMerge := misc.MergeMetadata(ts, ts.Metadata)
	if errMerge != nil {
		return fmt.Errorf("failed to merge metadata: %w", errMerge)
	}

	encoder := json.NewEncoder(f)
	encoder.SetIndent("", "  ")
	if err = encoder.Encode(data); err != nil {
		return fmt.Errorf("failed to write token to file: %w", err)
	}
	return nil
}

// IsExpired checks if the token has expired.
func (ts *CodebuddyTokenStorage) IsExpired() bool {
	if ts.Expired == "" {
		// Try ExpiresAt as Unix timestamp
		if ts.ExpiresAt > 0 {
			return time.Now().Unix() > ts.ExpiresAt
		}
		return false
	}
	t, err := time.Parse(time.RFC3339, ts.Expired)
	if err != nil {
		return true
	}
	return time.Now().Add(5 * time.Minute).After(t)
}

// NeedsRefresh checks if the token should be refreshed.
func (ts *CodebuddyTokenStorage) NeedsRefresh() bool {
	if ts.RefreshToken == "" {
		return false
	}
	return ts.IsExpired()
}
