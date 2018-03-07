package plugin

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2017-12-01/compute"

	"github.com/hashicorp/errwrap"
	"github.com/hashicorp/vault/helper/strutil"
	"github.com/hashicorp/vault/logical"
	"github.com/hashicorp/vault/logical/framework"
)

func pathLogin(b *azureAuthBackend) *framework.Path {
	return &framework.Path{
		Pattern: "login$",
		Fields: map[string]*framework.FieldSchema{
			"role": &framework.FieldSchema{
				Type:        framework.TypeString,
				Description: `The token role.`,
			},
			"jwt": &framework.FieldSchema{
				Type:        framework.TypeString,
				Description: `A signed JWT`,
			},
			"subscription_id": &framework.FieldSchema{
				Type:        framework.TypeString,
				Description: `The subscription id for the instance.`,
			},
			"resource_group_name": &framework.FieldSchema{
				Type:        framework.TypeString,
				Description: `The resource group from the instance.`,
			},
			"vm_name": &framework.FieldSchema{
				Type:        framework.TypeString,
				Description: `The name of the virtual machine.`,
			},
		},

		Callbacks: map[logical.Operation]framework.OperationFunc{
			logical.UpdateOperation:         b.pathLogin,
			logical.AliasLookaheadOperation: b.pathLogin,
		},

		//HelpSynopsis:    pathLoginHelpSyn,
		//HelpDescription: pathLoginHelpDesc,
	}
}

func (b *azureAuthBackend) pathLogin(ctx context.Context, req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
	signedJwt := data.Get("jwt").(string)
	if signedJwt == "" {
		return logical.ErrorResponse("jwt is required"), nil
	}
	roleName := data.Get("role").(string)
	if roleName == "" {
		return logical.ErrorResponse("role is required"), nil
	}
	subscriptionID := data.Get("subscription_id").(string)
	resourceGroupName := data.Get("resource_group_name").(string)
	vmName := data.Get("vm_name").(string)

	config, err := b.config(ctx, req.Storage)
	if err != nil {
		return nil, errwrap.Wrapf("unable to retrieve backend configuration: {{err}}", err)
	}
	if config == nil {
		return logical.ErrorResponse("backend not configured"), nil
	}

	role, err := b.role(ctx, req.Storage, roleName)
	if err != nil {
		return nil, err
	}
	if role == nil {
		return logical.ErrorResponse(fmt.Sprintf("invalid role name %q", roleName)), nil
	}

	// Set the client id for 'aud' claim verification
	provider, err := b.getProvider(config)
	if err != nil {
		return nil, err
	}

	// The OIDC verifier verifies the signature and checks the 'aud' and 'iss'
	// claims and expiration time
	idToken, err := provider.Verifier().Verify(ctx, signedJwt)
	if err != nil {
		return nil, err
	}

	claims := new(additionalClaims)
	if err := idToken.Claims(claims); err != nil {
		return nil, err
	}

	// Check additional claims in token
	if err := b.verifyClaims(claims, role); err != nil {
		return nil, err
	}

	if err := b.verifyResource(ctx, subscriptionID, resourceGroupName, vmName, claims, role); err != nil {
		return nil, err
	}

	resp := &logical.Response{
		Auth: &logical.Auth{
			Policies:    role.Policies,
			DisplayName: idToken.Subject,
			Period:      role.Period,
			NumUses:     role.NumUses,
			Alias: &logical.Alias{
				Name: idToken.Subject,
			},
			InternalData: map[string]interface{}{
				"role": roleName,
			},
			Metadata: map[string]string{
				"role": roleName,
			},
			LeaseOptions: logical.LeaseOptions{
				Renewable: true,
				TTL:       role.TTL,
			},
		},
	}
	return resp, nil
}

func (b *azureAuthBackend) verifyClaims(claims *additionalClaims, role *azureRole) error {
	notBefore := time.Time(claims.NotBefore)
	if notBefore.After(time.Now()) {
		return fmt.Errorf("token is not yet valid (Token Not Before: %v)", notBefore)
	}

	if len(role.BoundServicePrincipalIDs) > 0 {
		if !strutil.StrListContains(role.BoundServicePrincipalIDs, claims.ObjectID) {
			return fmt.Errorf("service principal not authorized: %s", claims.ObjectID)
		}
	}

	if len(role.BoundServicePrincipalIDs) > 0 {
		var found bool
		for _, group := range claims.GroupIDs {
			if !strutil.StrListContains(role.BoundGroupIDs, group) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("group not authorized: %v", claims.GroupIDs)
		}
	}

	return nil
}

func (b *azureAuthBackend) verifyResource(ctx context.Context, subscriptionID, resourceGroupName, vmName string, claims *additionalClaims, role *azureRole) error {
	// If not checking anythign with the resource id, exit early
	if len(role.BoundResourceGroups) == 0 && len(role.BoundSubscriptionsIDs) == 0 && len(role.BoundLocations) == 0 {
		return nil
	}

	if subscriptionID == "" || resourceGroupName == "" || vmName == "" {
		return errors.New("subscription_id, resource_group_name, and vm_name are required")
	}

	client := b.provider.ComputeClient(subscriptionID)
	vm, err := client.Get(ctx, resourceGroupName, vmName, compute.InstanceView)
	if err != nil {
		return errwrap.Wrapf("unable to retrieve virtual machine metadata: {{err}}", err)
	}

	// Ensure the principal id for the VM matches the verified token OID
	if vm.Identity == nil {
		return fmt.Errorf("vm client did not return identity information")
	}
	if vm.Identity.PrincipalID == nil {
		return fmt.Errorf("vm principal id is empty")
	}
	if *vm.Identity.PrincipalID != claims.ObjectID {
		return fmt.Errorf("token object id does not match virtual machine principal id")
	}

	// Check bound subsriptions
	if len(role.BoundSubscriptionsIDs) > 0 && !strutil.StrListContains(role.BoundSubscriptionsIDs, subscriptionID) {
		return fmt.Errorf("subscription not authoirzed")
	}

	// Check bound resource groups
	if len(role.BoundResourceGroups) > 0 && !strutil.StrListContains(role.BoundResourceGroups, resourceGroupName) {
		return fmt.Errorf("resource group not authoirzed")
	}

	// Check bound locations
	if len(role.BoundLocations) > 0 {
		if vm.Location == nil {
			return fmt.Errorf("vm location is empty")
		}
		if !strutil.StrListContains(role.BoundLocations, *vm.Location) {
			return fmt.Errorf("token object id does not match virtual machine principal id")
		}
	}

	return nil
}

func (b *azureAuthBackend) pathLoginRenew(ctx context.Context, req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
	roleName := req.Auth.InternalData["role"].(string)
	if roleName == "" {
		return nil, fmt.Errorf("failed to fetch role_name during renewal")
	}

	// Ensure that the Role still exists.
	role, err := b.role(ctx, req.Storage, roleName)
	if err != nil {
		return nil, fmt.Errorf("failed to validate role %s during renewal:%s", roleName, err)
	}
	if role == nil {
		return nil, fmt.Errorf("role %s does not exist during renewal", roleName)
	}

	// If 'Period' is set on the Role, the token should never expire.
	// Replenish the TTL with 'Period's value.
	if role.Period > time.Duration(0) {
		// If 'Period' was updated after the token was issued,
		// token will bear the updated 'Period' value as its TTL.
		req.Auth.TTL = role.Period
		return &logical.Response{Auth: req.Auth}, nil
	}

	return framework.LeaseExtend(role.TTL, role.MaxTTL, b.System())(ctx, req, data)
}

type additionalClaims struct {
	NotBefore jsonTime `json:"nbf"`
	ObjectID  string   `json:"oid"`
	GroupIDs  []string `json:"groups"`
}
