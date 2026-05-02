package auth

import (
	"context"
	"fmt"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/auth/codebuddy"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/browser"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// codebuddyRefreshLead is the duration before token expiry when refresh should occur.
var codebuddyRefreshLead = 5 * time.Minute

// CodebuddyAuthenticator implements the OAuth device flow login for Tencent Codebuddy.
type CodebuddyAuthenticator struct{}

// NewCodebuddyAuthenticator constructs a new Codebuddy authenticator.
func NewCodebuddyAuthenticator() Authenticator {
	return &CodebuddyAuthenticator{}
}

// Provider returns the provider key for codebuddy.
func (CodebuddyAuthenticator) Provider() string {
	return "codebuddy"
}

// CodebuddyIntlAuthenticator handles auth for the international version (www.codebuddy.ai).
// It reuses the same OAuth flow but registers under a different provider key.
type CodebuddyIntlAuthenticator struct {
	CodebuddyAuthenticator
}

// NewCodebuddyIntlAuthenticator constructs a new Codebuddy Intl authenticator.
func NewCodebuddyIntlAuthenticator() Authenticator {
	return &CodebuddyIntlAuthenticator{}
}

// Provider returns the provider key for codebuddy-intl.
func (CodebuddyIntlAuthenticator) Provider() string {
	return "codebuddy-intl"
}

// RefreshLead returns the duration before token expiry when refresh should occur.
func (CodebuddyAuthenticator) RefreshLead() *time.Duration {
	return &codebuddyRefreshLead
}

// Login initiates the Codebuddy OAuth device flow authentication.
func (a CodebuddyAuthenticator) Login(ctx context.Context, cfg *config.Config, opts *LoginOptions) (*coreauth.Auth, error) {
	if cfg == nil {
		return nil, fmt.Errorf("cliproxy auth: configuration is required")
	}
	if opts == nil {
		opts = &LoginOptions{}
	}

	authSvc := codebuddy.NewCodebuddyAuth(cfg)

	// Start the OAuth flow
	fmt.Println("Starting Codebuddy authentication...")
	deviceCode, err := authSvc.StartLogin(ctx)
	if err != nil {
		return nil, fmt.Errorf("codebuddy: failed to start login: %w", err)
	}

	// Display the verification URL
	verificationURL := deviceCode.VerificationURIComplete
	if verificationURL == "" {
		verificationURL = deviceCode.VerificationURI
	}

	fmt.Printf("\nTo authenticate with Codebuddy, please visit:\n%s\n\n", verificationURL)

	// Try to open the browser automatically
	if !opts.NoBrowser {
		if browser.IsAvailable() {
			if errOpen := browser.OpenURL(verificationURL); errOpen != nil {
				log.Warnf("Failed to open browser automatically: %v", errOpen)
			} else {
				fmt.Println("Browser opened automatically.")
			}
		}
	}

	fmt.Println("Waiting for authorization...")
	if deviceCode.ExpiresIn > 0 {
		fmt.Printf("(This will timeout in %d seconds if not authorized)\n", deviceCode.ExpiresIn)
	}

	// Wait for user authorization
	authBundle, err := authSvc.WaitForAuthorization(ctx, deviceCode)
	if err != nil {
		return nil, fmt.Errorf("codebuddy: %w", err)
	}

	// Build token storage
	var expired string
	if authBundle.TokenData.ExpiresAt > 0 {
		expired = time.Unix(authBundle.TokenData.ExpiresAt, 0).UTC().Format(time.RFC3339)
	}

	tokenStorage := &codebuddy.CodebuddyTokenStorage{
		AccessToken:  authBundle.TokenData.AccessToken,
		RefreshToken: authBundle.TokenData.RefreshToken,
		TokenType:    authBundle.TokenData.TokenType,
		ExpiresAt:    authBundle.TokenData.ExpiresAt,
		Domain:       authBundle.TokenData.Domain,
		Expired:      expired,
		Type:         "codebuddy",
	}

	label := "Codebuddy User"
	if authBundle.AccountInfo != nil {
		if authBundle.AccountInfo.Email != "" {
			label = authBundle.AccountInfo.Email
			tokenStorage.Email = authBundle.AccountInfo.Email
		}
		if authBundle.AccountInfo.UID != "" {
			tokenStorage.UID = authBundle.AccountInfo.UID
		}
		if authBundle.AccountInfo.Nickname != "" {
			tokenStorage.Nickname = authBundle.AccountInfo.Nickname
		}
	}

	// Build metadata
	metadata := map[string]any{
		"type":          "codebuddy",
		"access_token":  authBundle.TokenData.AccessToken,
		"refresh_token": authBundle.TokenData.RefreshToken,
		"token_type":    authBundle.TokenData.TokenType,
		"expires_at":    authBundle.TokenData.ExpiresAt,
		"domain":        authBundle.TokenData.Domain,
		"timestamp":     time.Now().UnixMilli(),
	}
	if authBundle.AccountInfo != nil && authBundle.AccountInfo.UID != "" {
		metadata["uid"] = authBundle.AccountInfo.UID
	}
	if expired != "" {
		metadata["expired"] = expired
	}

	// Generate a unique filename
	fileName := fmt.Sprintf("codebuddy-%d.json", time.Now().UnixMilli())

	fmt.Println("\nCodebuddy authentication successful!")

	return &coreauth.Auth{
		ID:       fileName,
		Provider: a.Provider(),
		FileName: fileName,
		Label:    label,
		Storage:  tokenStorage,
		Metadata: metadata,
	}, nil
}
