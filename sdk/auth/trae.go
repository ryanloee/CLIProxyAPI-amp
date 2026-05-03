package auth

import (
	"context"
	"fmt"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/auth/trae"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/browser"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

var traeRefreshLead = 5 * time.Minute

// TraeAuthenticator implements the OAuth login flow for Trae international.
type TraeAuthenticator struct{}

// NewTraeAuthenticator constructs a new Trae authenticator.
func NewTraeAuthenticator() Authenticator {
	return &TraeAuthenticator{}
}

// Provider returns the provider key for trae.
func (TraeAuthenticator) Provider() string {
	return "trae"
}

// RefreshLead returns the duration before token expiry when refresh should occur.
func (TraeAuthenticator) RefreshLead() *time.Duration {
	return &traeRefreshLead
}

// Login initiates the Trae OAuth authentication.
func (a TraeAuthenticator) Login(ctx context.Context, cfg *config.Config, opts *LoginOptions) (*coreauth.Auth, error) {
	if cfg == nil {
		return nil, fmt.Errorf("cliproxy auth: configuration is required")
	}
	if opts == nil {
		opts = &LoginOptions{}
	}

	authSvc := trae.NewTraeAuth(cfg)

	fmt.Println("Starting Trae authentication...")
	deviceCode, err := authSvc.StartLogin(ctx)
	if err != nil {
		return nil, fmt.Errorf("trae: failed to start login: %w", err)
	}

	verificationURL := deviceCode.VerificationURIComplete
	if verificationURL == "" {
		verificationURL = deviceCode.VerificationURI
	}

	fmt.Printf("\nTo authenticate with Trae, please visit:\n%s\n\n", verificationURL)

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

	authBundle, err := authSvc.WaitForAuthorization(ctx, deviceCode)
	if err != nil {
		return nil, fmt.Errorf("trae: %w", err)
	}

	var expired string
	if authBundle.TokenData.ExpiresAt > 0 {
		expired = time.Unix(authBundle.TokenData.ExpiresAt, 0).UTC().Format(time.RFC3339)
	}

	tokenStorage := &trae.TraeTokenStorage{
		AccessToken:  authBundle.TokenData.AccessToken,
		RefreshToken: authBundle.TokenData.RefreshToken,
		TokenType:    authBundle.TokenData.TokenType,
		ExpiresAt:    authBundle.TokenData.ExpiresAt,
		LoginHost:    authBundle.TokenData.LoginHost,
		LoginRegion:  authBundle.LoginRegion,
		Expired:      expired,
		Type:         "trae",
	}

	label := "Trae User"
	if authBundle.AccountInfo != nil {
		if authBundle.AccountInfo.Email != "" {
			label = authBundle.AccountInfo.Email
			tokenStorage.Email = authBundle.AccountInfo.Email
		}
		if authBundle.AccountInfo.UserID != "" {
			tokenStorage.UserID = authBundle.AccountInfo.UserID
		}
		if authBundle.AccountInfo.Nickname != "" {
			tokenStorage.Nickname = authBundle.AccountInfo.Nickname
		}
	}

	metadata := map[string]any{
		"type":          "trae",
		"access_token":  authBundle.TokenData.AccessToken,
		"refresh_token": authBundle.TokenData.RefreshToken,
		"token_type":    authBundle.TokenData.TokenType,
		"expires_at":    authBundle.TokenData.ExpiresAt,
		"login_host":    authBundle.TokenData.LoginHost,
		"login_region":  authBundle.LoginRegion,
		"timestamp":     time.Now().UnixMilli(),
	}
	if authBundle.AccountInfo != nil && authBundle.AccountInfo.UserID != "" {
		metadata["user_id"] = authBundle.AccountInfo.UserID
	}
	if expired != "" {
		metadata["expired"] = expired
	}

	fileName := fmt.Sprintf("trae-%d.json", time.Now().UnixMilli())

	fmt.Println("\nTrae authentication successful!")

	return &coreauth.Auth{
		ID:       fileName,
		Provider: a.Provider(),
		FileName: fileName,
		Label:    label,
		Storage:  tokenStorage,
		Metadata: metadata,
	}, nil
}
