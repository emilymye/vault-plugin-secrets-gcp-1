package gcpsecrets

import (
	"context"
	"fmt"
	"github.com/hashicorp/vault/sdk/framework"

	"github.com/hashicorp/errwrap"
	"github.com/hashicorp/go-gcp-common/gcputil"
	"github.com/hashicorp/go-multierror"
	"github.com/hashicorp/vault-plugin-secrets-gcp/plugin/iamutil"
	"github.com/hashicorp/vault-plugin-secrets-gcp/plugin/util"
	"github.com/hashicorp/vault/sdk/helper/useragent"
	"github.com/hashicorp/vault/sdk/logical"
	"github.com/mitchellh/mapstructure"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iam/v1"
)

const (
	walTypeAccount    = "account"
	walTypeAccountKey = "account_key"
	walTypeIamPolicy  = "iam_policy"
)

func (b *backend) walRollback(ctx context.Context, req *logical.Request, kind string, data interface{}) error {
	switch kind {
	case walTypeAccount:
		return b.serviceAccountRollback(ctx, req, data)
	case walTypeAccountKey:
		return b.serviceAccountKeyRollback(ctx, req, data)
	case walTypeIamPolicy:
		return b.serviceAccountPolicyRollback(ctx, req, data)
	default:
		return fmt.Errorf("unknown type to rollback")
	}
}

type walAccount struct {
	RoleSet string
	Id      gcputil.ServiceAccountId
}

type walAccountKey struct {
	RoleSet            string
	ServiceAccountName string
	KeyName            string
}

type walIamPolicy struct {
	RoleSet   string
	AccountId gcputil.ServiceAccountId
	Resource  string
	Roles     []string
}

func (b *backend) serviceAccountRollback(ctx context.Context, req *logical.Request, data interface{}) error {
	b.rolesetLock.Lock()
	defer b.rolesetLock.Unlock()

	var entry walAccount
	if err := mapstructure.Decode(data, &entry); err != nil {
		return err
	}

	// If account is still being used, WAL entry was not
	// deleted properly after a successful operation.
	// Remove WAL entry.
	rs, err := getRoleSet(entry.RoleSet, ctx, req.Storage)
	if err != nil {
		return err
	}
	if rs != nil && entry.Id.ResourceName() == rs.AccountId.ResourceName() {
		// Still being used - don't delete this service account.
		return nil
	}

	// Delete service account.
	iamC, err := b.IAMAdminClient(req.Storage)
	if err != nil {
		return err
	}

	return b.deleteServiceAccount(ctx, iamC, entry.Id)
}

func (b *backend) serviceAccountKeyRollback(ctx context.Context, req *logical.Request, data interface{}) error {
	b.rolesetLock.Lock()
	defer b.rolesetLock.Unlock()

	var entry walAccountKey
	if err := mapstructure.Decode(data, &entry); err != nil {
		return err
	}

	b.Logger().Debug("checking roleset listed in WAL is access_token roleset")

	var keyInUse string

	// Get roleset for entry
	rs, err := getRoleSet(entry.RoleSet, ctx, req.Storage)
	if err != nil {
		return err
	}

	// If roleset is not nil, get key in use.
	if rs != nil {
		if rs.SecretType != SecretTypeAccessToken {
			// Don't clean keys if roleset doesn't create access_tokens (i.e. creates keys).
			return nil
		}

		if rs.TokenGen != nil {
			keyInUse = rs.TokenGen.KeyName
		}
	}

	iamC, err := b.IAMAdminClient(req.Storage)
	if err != nil {
		return err
	}

	if entry.KeyName == "" {
		// If given an empty key name, this means the WAL entry was created before the key was created.
		// We list all keys and then delete any not in use by the current roleset.
		keys, err := iamC.Projects.ServiceAccounts.Keys.List(entry.ServiceAccountName).KeyTypes("USER_MANAGED").Do()
		if err != nil {
			// If service account already deleted, no need to clean up keys.
			if isGoogleAccountNotFoundErr(err) {
				return nil
			}
			return err
		}

		for _, k := range keys.Keys {
			// Skip deleting keys still in use (empty keyInUse means no key is in use)
			if k.Name == keyInUse {
				continue
			}

			_, err = iamC.Projects.ServiceAccounts.Keys.Delete(entry.KeyName).Do()
			if err != nil && !isGoogleAccountKeyNotFoundErr(err) {
				return err
			}
		}
		return nil
	}

	// If key is still in use, don't delete (empty keyInUse means no key is in use)
	if entry.KeyName == keyInUse {
		return nil
	}

	_, err = iamC.Projects.ServiceAccounts.Keys.Delete(entry.KeyName).Do()
	if err != nil && !isGoogleAccountKeyNotFoundErr(err) {
		return err
	}
	return nil
}

func (b *backend) serviceAccountPolicyRollback(ctx context.Context, req *logical.Request, data interface{}) error {
	b.rolesetLock.Lock()
	defer b.rolesetLock.Unlock()

	var entry walIamPolicy
	if err := mapstructure.Decode(data, &entry); err != nil {
		return err
	}

	// Try to verify service account not being used by roleset
	rs, err := getRoleSet(entry.RoleSet, ctx, req.Storage)
	if err != nil {
		return err
	}

	// Take out any bindings still being used by this role set from roles being removed.
	rolesToRemove := util.ToSet(entry.Roles)
	if rs != nil && rs.AccountId.ResourceName() == entry.AccountId.ResourceName() {
		currRoles, ok := rs.Bindings[entry.Resource]
		if ok {
			rolesToRemove = rolesToRemove.Sub(currRoles)
		}
	}

	r, err := b.iamResources.Parse(entry.Resource)
	if err != nil {
		return err
	}

	httpC, err := b.HTTPClient(req.Storage)
	if err != nil {
		return err
	}

	iamHandle := iamutil.GetIamHandle(httpC, useragent.String())
	if err != nil {
		return err
	}

	p, err := iamHandle.GetIamPolicy(ctx, r)
	if err != nil {
		return err
	}

	changed, newP := p.RemoveBindings(
		&iamutil.PolicyDelta{
			Email: entry.AccountId.EmailOrId,
			Roles: rolesToRemove,
		})

	if !changed {
		return nil
	}

	_, err = iamHandle.SetIamPolicy(ctx, r, newP)
	return err
}

func (b *backend) deleteServiceAccount(ctx context.Context, iamAdmin *iam.Service, account gcputil.ServiceAccountId) error {
	if account.EmailOrId == "" {
		return nil
	}

	_, err := iamAdmin.Projects.ServiceAccounts.Delete(account.ResourceName()).Do()
	if err != nil && !isGoogleAccountNotFoundErr(err) {
		return errwrap.Wrapf("unable to delete service account: {{err}}", err)
	}
	return nil
}

func (b *backend) deleteTokenGenKey(ctx context.Context, iamAdmin *iam.Service, tgen *TokenGenerator) error {
	if tgen == nil || tgen.KeyName == "" {
		return nil
	}

	_, err := iamAdmin.Projects.ServiceAccounts.Keys.Delete(tgen.KeyName).Do()
	if err != nil && !isGoogleAccountKeyNotFoundErr(err) {
		return errwrap.Wrapf("unable to delete service account key: {{err}}", err)
	}
	return nil
}

func (b *backend) removeBindings(ctx context.Context, iamHandle *iamutil.IamHandle, email string, bindings ResourceBindings) (allErr *multierror.Error) {
	for resName, roles := range bindings {
		resource, err := b.iamResources.Parse(resName)
		if err != nil {
			allErr = multierror.Append(allErr, errwrap.Wrapf(fmt.Sprintf("unable to delete role binding for resource '%s': {{err}}", resName), err))
			continue
		}

		p, err := iamHandle.GetIamPolicy(ctx, resource)
		if err != nil {
			allErr = multierror.Append(allErr, errwrap.Wrapf(fmt.Sprintf("unable to delete role binding for resource '%s': {{err}}", resName), err))
			continue
		}

		changed, newP := p.RemoveBindings(&iamutil.PolicyDelta{
			Email: email,
			Roles: roles,
		})
		if !changed {
			continue
		}
		if _, err = iamHandle.SetIamPolicy(ctx, resource, newP); err != nil {
			allErr = multierror.Append(allErr, errwrap.Wrapf(fmt.Sprintf("unable to delete role binding for resource '%s': {{err}}", resName), err))
			continue
		}
	}
	return
}

// This tries to clean up WALs that are no longer needed.
// We can ignore errors if deletion fails as WAL rollback will no-op if the object is still in use or no longer exists.
// This simply attempts to reduce the number of GCP calls we will trigger in rollbacks.
func (b *backend) tryDeleteWALs(ctx context.Context, s logical.Storage, walIds ...string) {
	for _, walId := range walIds {
		// ignore errors, WALs that are not needed will just no-op
		err := framework.DeleteWAL(ctx, s, walId)
		if err != nil {
			b.Logger().Error("unable to delete unneeded WAL %s, ignoring error since WAL will no-op: %v", walId, err)
		}
	}
}

func isGoogleAccountNotFoundErr(err error) bool {
	return isGoogleApiErrorWithCodes(err, 404)
}

func isGoogleAccountKeyNotFoundErr(err error) bool {
	return isGoogleApiErrorWithCodes(err, 403, 404)
}

func isGoogleApiErrorWithCodes(err error, validErrCodes ...int) bool {
	if err == nil {
		return false
	}
	gErr, ok := err.(*googleapi.Error)
	if !ok {
		return false
	}

	for _, code := range validErrCodes {
		if gErr.Code == code {
			return true
		}
	}

	return false
}
