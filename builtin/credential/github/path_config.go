package github

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/google/go-github/github"
	"github.com/hashicorp/vault/sdk/framework"
	"github.com/hashicorp/vault/sdk/helper/tokenutil"
	"github.com/hashicorp/vault/sdk/logical"
)

func pathConfig(b *backend) *framework.Path {
	p := &framework.Path{
		Pattern: "config",
		Fields: map[string]*framework.FieldSchema{
			"organization": {
				Type:        framework.TypeString,
				Description: "The organization users must be part of",
				Required:    true,
			},
			"organization_id": {
				Type:        framework.TypeInt64,
				Description: "The ID of the organization users must be part of",
			},
			"base_url": {
				Type: framework.TypeString,
				Description: `The API endpoint to use. Useful if you
are running GitHub Enterprise or an
API-compatible authentication server.`,
				DisplayAttrs: &framework.DisplayAttributes{
					Name:  "Base URL",
					Group: "GitHub Options",
				},
			},
			"ttl": {
				Type:        framework.TypeDurationSecond,
				Description: tokenutil.DeprecationText("token_ttl"),
				Deprecated:  true,
			},
			"max_ttl": {
				Type:        framework.TypeDurationSecond,
				Description: tokenutil.DeprecationText("token_max_ttl"),
				Deprecated:  true,
			},
		},

		Callbacks: map[logical.Operation]framework.OperationFunc{
			logical.UpdateOperation: b.pathConfigWrite,
			logical.ReadOperation:   b.pathConfigRead,
		},
	}

	tokenutil.AddTokenFields(p.Fields)
	p.Fields["token_policies"].Description += ". This will apply to all tokens generated by this auth method, in addition to any policies configured for specific users/groups."
	return p
}

func (b *backend) pathConfigWrite(ctx context.Context, req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
	var resp logical.Response
	c, err := b.Config(ctx, req.Storage)
	if err != nil {
		return nil, err
	}
	if c == nil {
		c = &config{}
	}

	if organizationRaw, ok := data.GetOk("organization"); ok {
		c.Organization = organizationRaw.(string)
	}
	if c.Organization == "" {
		return logical.ErrorResponse("organization is a required parameter"), nil
	}

	if organizationRaw, ok := data.GetOk("organization_id"); ok {
		c.OrganizationID = organizationRaw.(int64)
	}

	var parsedURL *url.URL
	if baseURLRaw, ok := data.GetOk("base_url"); ok {
		baseURL := baseURLRaw.(string)
		if !strings.HasSuffix(baseURL, "/") {
			baseURL += "/"
		}
		parsedURL, err = url.Parse(baseURL)
		if err != nil {
			return logical.ErrorResponse(fmt.Sprintf("error parsing given base_url: %s", err)), nil
		}
		c.BaseURL = baseURL
	}

	if c.OrganizationID == 0 {
		githubToken := os.Getenv("VAULT_AUTH_CONFIG_GITHUB_TOKEN")
		client, err := b.Client(githubToken)
		if err != nil {
			return nil, err
		}
		// ensure our client has the BaseURL if it was provided
		if parsedURL != nil {
			client.BaseURL = parsedURL
		}

		// we want to set the Org ID in the config so we can use that to verify
		// the credentials on login
		err = c.setOrganizationID(ctx, client)
		if err != nil {
			errorMsg := fmt.Errorf("unable to fetch the organization_id, you must manually set it in the config: %s", err)
			b.Logger().Error(errorMsg.Error())
			return nil, errorMsg
		}
	}

	if err := c.ParseTokenFields(req, data); err != nil {
		return logical.ErrorResponse(err.Error()), logical.ErrInvalidRequest
	}

	// Handle upgrade cases
	{
		if err := tokenutil.UpgradeValue(data, "ttl", "token_ttl", &c.TTL, &c.TokenTTL); err != nil {
			return logical.ErrorResponse(err.Error()), nil
		}

		if err := tokenutil.UpgradeValue(data, "max_ttl", "token_max_ttl", &c.MaxTTL, &c.TokenMaxTTL); err != nil {
			return logical.ErrorResponse(err.Error()), nil
		}
	}

	entry, err := logical.StorageEntryJSON("config", c)
	if err != nil {
		return nil, err
	}

	if err := req.Storage.Put(ctx, entry); err != nil {
		return nil, err
	}

	if len(resp.Warnings) == 0 {
		return nil, nil
	}

	return &resp, nil
}

func (b *backend) pathConfigRead(ctx context.Context, req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
	config, err := b.Config(ctx, req.Storage)
	if err != nil {
		return nil, err
	}
	if config == nil {
		return nil, nil
	}

	d := map[string]interface{}{
		"organization_id": config.OrganizationID,
		"organization":    config.Organization,
		"base_url":        config.BaseURL,
	}
	config.PopulateTokenData(d)

	if config.TTL > 0 {
		d["ttl"] = int64(config.TTL.Seconds())
	}
	if config.MaxTTL > 0 {
		d["max_ttl"] = int64(config.MaxTTL.Seconds())
	}

	return &logical.Response{
		Data: d,
	}, nil
}

// Config returns the configuration for this backend.
func (b *backend) Config(ctx context.Context, s logical.Storage) (*config, error) {
	entry, err := s.Get(ctx, "config")
	if err != nil {
		return nil, err
	}
	if entry == nil {
		return nil, nil
	}

	var result config
	if entry != nil {
		if err := entry.DecodeJSON(&result); err != nil {
			return nil, fmt.Errorf("error reading configuration: %w", err)
		}
	}

	if result.TokenTTL == 0 && result.TTL > 0 {
		result.TokenTTL = result.TTL
	}
	if result.TokenMaxTTL == 0 && result.MaxTTL > 0 {
		result.TokenMaxTTL = result.MaxTTL
	}

	return &result, nil
}

type config struct {
	tokenutil.TokenParams

	OrganizationID int64         `json:"organization_id" structs:"organization_id" mapstructure:"organization_id"`
	Organization   string        `json:"organization" structs:"organization" mapstructure:"organization"`
	BaseURL        string        `json:"base_url" structs:"base_url" mapstructure:"base_url"`
	TTL            time.Duration `json:"ttl" structs:"ttl" mapstructure:"ttl"`
	MaxTTL         time.Duration `json:"max_ttl" structs:"max_ttl" mapstructure:"max_ttl"`
}

func (c *config) setOrganizationID(ctx context.Context, client *github.Client) error {
	org, _, err := client.Organizations.Get(ctx, c.Organization)
	if err != nil {
		return err
	}

	orgID := org.GetID()
	if orgID == 0 {
		return fmt.Errorf("organization_id not found for %s", c.Organization)
	}

	c.OrganizationID = orgID

	return nil
}
