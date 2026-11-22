package main

import (
	"context"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/iameval"
	"github.com/cloudburrow/cloudburrow/internal/metadata"
	"github.com/cloudburrow/cloudburrow/internal/registry"
)

// registryTokenTTL is how long a token the registry's token endpoint
// mints for a key file is good for: an hour, as Google's access tokens.
const registryTokenTTL = time.Hour

// useRegistryAuth gives the Docker registry its token auth (#2111): a
// login with an access token this instance minted (oauth2accesstoken, or a
// credential helper's username) is issued that token, and one with a key
// file the instance holds (_json_key, _json_key_base64) a token minted for
// the key's account. With --iam-enforcement=on, credentials the instance
// does not accept are refused, and each request is checked against the
// repository's policy and its project's, through the one evaluator, as
// Artifact Registry's API is; with it off, any credentials are taken and
// nothing is checked, as for every other service.
func useRegistryAuth(e *iameval.Enforcer, creds *metadata.Credentials) {
	on := func() bool { return e != nil && e.On != nil && e.On() }
	runadapter.UseRegistryAuth(&registry.Auth{
		Required: on,
		Login: func(user, password string) (string, error) {
			if registry.IsAccessTokenUser(user) {
				if creds != nil {
					if _, ok := creds.TokenAccount(password, time.Now()); ok {
						return password, nil
					}
				}
			} else if key, ok := registry.KeyCredential(user, password); ok && creds != nil {
				if email, ok := creds.KeyFileAccount(key, time.Now()); ok {
					return creds.MintAccessTokenFor(email, registryTokenTTL), nil
				}
			}
			if on() {
				return "", registry.ErrUnauthenticated
			}
			return registry.AnonymousToken, nil
		},
		Authorize: func(bearer, permission, resource string) error {
			if !on() {
				return nil
			}
			if permission == "" {
				// Only the token's validity: the ping.
				if bearer != "" && e.CallerOf != nil && e.CallerOf(bearer).Invalid {
					return registry.ErrUnauthenticated
				}
				return nil
			}
			err := e.Authorize(context.Background(), iameval.ArtifactRegistry, bearer, permission, resource)
			switch status.Code(err) {
			case codes.OK:
				return nil
			case codes.Unauthenticated:
				return registry.ErrUnauthenticated
			}
			return &registry.DeniedError{Permission: permission, Resource: resource}
		},
	})
}
