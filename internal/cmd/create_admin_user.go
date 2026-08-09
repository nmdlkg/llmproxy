package cmd

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/tenancy"
)

const defaultAdminUserTier = "default"

type adminUserBootstrapResult struct {
	email     string
	plaintext string
	existing  bool
}

// DoCreateAdminUser creates the first tenancy admin, or issues a recovery key
// when the email already belongs to a user.
func DoCreateAdminUser(cfg *config.Config, email, tier string, output io.Writer) error {
	if cfg == nil || !cfg.Tenancy.Enabled {
		return fmt.Errorf("create-admin-user: tenancy.enabled must be true in the loaded config")
	}
	if output == nil {
		return fmt.Errorf("create-admin-user: output is nil")
	}

	normalizedEmail := strings.TrimSpace(email)
	if normalizedEmail == "" {
		return fmt.Errorf("create-admin-user: email is empty")
	}
	normalizedTier := strings.ToLower(strings.TrimSpace(tier))
	if normalizedTier == "" {
		normalizedTier = defaultAdminUserTier
	}

	result, errBootstrap := bootstrapAdminUser(cfg, normalizedEmail, normalizedTier)
	if errBootstrap != nil {
		return errBootstrap
	}
	if result.existing {
		if _, errWrite := fmt.Fprintf(output, "Admin user already exists for %s; issued a new recovery API key without creating a duplicate.\n", result.email); errWrite != nil {
			return fmt.Errorf("create-admin-user: write recovery status: %w", errWrite)
		}
	} else {
		if _, errWrite := fmt.Fprintf(output, "Admin user created for %s.\n", result.email); errWrite != nil {
			return fmt.Errorf("create-admin-user: write creation status: %w", errWrite)
		}
	}
	if _, errWrite := fmt.Fprintf(output, "UNRECOVERABLE API key (shown exactly once): %s\n", result.plaintext); errWrite != nil {
		return fmt.Errorf("create-admin-user: write API key: %w", errWrite)
	}
	return nil
}

func bootstrapAdminUser(cfg *config.Config, email, tier string) (_ *adminUserBootstrapResult, resultErr error) {
	store, errOpen := tenancy.OpenSQLite(cfg.Tenancy, cfg.AuthDir)
	if errOpen != nil {
		return nil, fmt.Errorf("create-admin-user: open tenancy store: %w", errOpen)
	}
	defer func() {
		if errClose := store.Close(); errClose != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("create-admin-user: close tenancy store: %w", errClose))
		}
	}()

	users, errList := store.ListUsers()
	if errList != nil {
		return nil, fmt.Errorf("create-admin-user: list tenancy users: %w", errList)
	}
	var user *tenancy.User
	for index := range users {
		if strings.EqualFold(strings.TrimSpace(users[index].Email), email) {
			user = &users[index]
			break
		}
	}

	existing := user != nil
	if !existing {
		user = &tenancy.User{
			Email:       email,
			DisplayName: email,
			Role:        tenancy.RoleAdmin,
			Tier:        tier,
		}
		if errCreate := store.CreateUser(user); errCreate != nil {
			return nil, fmt.Errorf("create-admin-user: create admin user: %w", errCreate)
		}
	}

	label := "admin bootstrap"
	if existing {
		label = "admin recovery"
	}
	plaintext, _, errIssue := store.IssueAPIKey(user.ID, label)
	if errIssue != nil {
		if !existing {
			if errDelete := store.DeleteUser(user.ID); errDelete != nil {
				errIssue = errors.Join(errIssue, fmt.Errorf("remove admin user after key issuance failure: %w", errDelete))
			}
		}
		return nil, fmt.Errorf("create-admin-user: issue API key: %w", errIssue)
	}
	return &adminUserBootstrapResult{
		email:     user.Email,
		plaintext: plaintext,
		existing:  existing,
	}, nil
}
