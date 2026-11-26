package kube

import (
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// claims are the registered claims (RFC 7519 4.1) of the user's token that
// explain what a target apiserver makes of it: who issued it, for whom, until
// when.
type claims struct {
	Issuer   string
	Audience []string
	Expiry   time.Time
}

// signatureAlgorithms are the JWS algorithms whose tokens tokenClaims reads.
// The signature is not checked: mcp-oauth verified it when the request came
// in.
var signatureAlgorithms = []jose.SignatureAlgorithm{
	jose.RS256, jose.RS384, jose.RS512,
	jose.PS256, jose.PS384, jose.PS512,
	jose.ES256, jose.ES384, jose.ES512,
	jose.EdDSA,
}

// tokenClaims reads iss, aud and exp of a JWT without verifying it, for
// messages. The zero claims when the token is not a JWS.
func tokenClaims(token string) claims {
	parsed, err := jwt.ParseSigned(token, signatureAlgorithms)
	if err != nil {
		return claims{}
	}
	var registered jwt.Claims
	if err := parsed.UnsafeClaimsWithoutVerification(&registered); err != nil {
		return claims{}
	}
	return claims{Issuer: registered.Issuer, Audience: registered.Audience, Expiry: registered.Expiry.Time()}
}
