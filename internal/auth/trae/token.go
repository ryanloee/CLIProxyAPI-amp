package trae

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/misc"
)

// TraeTokenStorage stores OAuth token information for Trae API authentication.
type TraeTokenStorage struct {
	AccessToken  string         `json:"access_token"`
	RefreshToken string         `json:"refresh_token,omitempty"`
	TokenType    string         `json:"token_type,omitempty"`
	ExpiresAt    int64          `json:"expires_at,omitempty"`
	Expired      string         `json:"expired,omitempty"`
	LoginHost    string         `json:"login_host,omitempty"`
	LoginRegion  string         `json:"login_region,omitempty"`
	Type         string         `json:"type"`
	Email        string         `json:"email,omitempty"`
	UserID       string         `json:"user_id,omitempty"`
	Nickname     string         `json:"nickname,omitempty"`
	Metadata     map[string]any `json:"-"`
}

// SetMetadata allows external callers to inject metadata into the storage before saving.
func (ts *TraeTokenStorage) SetMetadata(meta map[string]any) {
	ts.Metadata = meta
}

// TraeTokenData holds the raw OAuth token response from Trae.
type TraeTokenData struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresAt    int64  `json:"expires_at"`
	LoginHost    string `json:"login_host"`
}

// TraeAccountInfo holds user account information from Trae.
type TraeAccountInfo struct {
	Email    string `json:"email"`
	UserID   string `json:"user_id"`
	Nickname string `json:"nickname"`
}

// TraeAuthBundle bundles authentication data for storage.
type TraeAuthBundle struct {
	TokenData   *TraeTokenData
	AccountInfo *TraeAccountInfo
	LoginHost   string
	LoginRegion string
}

// DeviceCodeResponse represents Trae's auth state response.
type DeviceCodeResponse struct {
	LoginID                 string
	State                   string
	VerificationURI         string
	VerificationURIComplete string
	ExpiresIn               int
	Interval                int
	CallbackPort            int
	CallbackResultCh        <-chan callbackResult
	LoginHost               string
}

// SaveTokenToFile serializes the Trae token storage to a JSON file.
func (ts *TraeTokenStorage) SaveTokenToFile(authFilePath string) error {
	misc.LogSavingCredentials(authFilePath)
	ts.Type = "trae"

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
func (ts *TraeTokenStorage) IsExpired() bool {
	if ts.Expired == "" {
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
func (ts *TraeTokenStorage) NeedsRefresh() bool {
	if ts.RefreshToken == "" {
		return false
	}
	return ts.IsExpired()
}
