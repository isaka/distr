package authinfo

import (
	"context"

	"github.com/glasskube/distr/internal/authjwt"
	"github.com/glasskube/distr/internal/authn"
	"github.com/glasskube/distr/internal/types"
	"github.com/glasskube/distr/internal/util"
	"github.com/lestrrat-go/jwx/v2/jwt"
)

func FromJWT(token jwt.Token) (*SimpleAuthInfo, error) {
	var result SimpleAuthInfo
	result.userID = token.Subject()
	result.rawToken = token
	if userEmail, ok := token.Get(authjwt.UserEmailKey); ok {
		result.userEmail = userEmail.(string)
	}
	if userRole, ok := token.Get(authjwt.UserRoleKey); ok {
		result.userRole = util.PtrTo(types.UserRole(userRole.(string)))
	}
	if orgId, ok := token.Get(authjwt.OrgIdKey); ok {
		result.organizationID = util.PtrTo(orgId.(string))
	}
	if verified, ok := token.Get(authjwt.UserEmailVerifiedKey); ok {
		result.emailVerified = verified.(bool)
	}
	return &result, nil
}

func JWTAuthenticator() authn.Authenticator[jwt.Token, AuthInfo] {
	return authn.AuthenticatorFunc[jwt.Token, AuthInfo](
		func(ctx context.Context, token jwt.Token) (AuthInfo, error) {
			return FromJWT(token)
		},
	)
}
