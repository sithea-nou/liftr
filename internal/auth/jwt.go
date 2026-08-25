// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"

	"github.com/sithea-nou/liftr/internal/identity"
)

// parsedToken is one structurally valid compact JWS serialization with its
// decoded parts. Verification happens against the raw signing input so no
// re-encoding can alter what is checked.
type parsedToken struct {
	signingInput string
	header       tokenHeader
	claims       map[string]json.RawMessage
	signature    []byte
}

type tokenHeader struct {
	Typ string `json:"typ"`
	Alg string `json:"alg"`
	Kid string `json:"kid"`
}

var encoding = base64.RawURLEncoding

// parseToken splits and decodes a compact JWS without trusting any of it.
func parseToken(credential string) (*parsedToken, error) {
	parts := strings.Split(credential, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return nil, invalid(identity.AuthFailureMalformed)
	}
	headerBytes, err := encoding.DecodeString(parts[0])
	if err != nil {
		return nil, invalid(identity.AuthFailureMalformed)
	}
	var header tokenHeader
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return nil, invalid(identity.AuthFailureMalformed)
	}
	if subtle.ConstantTimeCompare([]byte(header.Alg), []byte("none")) == 1 {
		return nil, invalid(identity.AuthFailureMalformed)
	}
	claimsBytes, err := encoding.DecodeString(parts[1])
	if err != nil {
		return nil, invalid(identity.AuthFailureMalformed)
	}
	var claims map[string]json.RawMessage
	if err := json.Unmarshal(claimsBytes, &claims); err != nil {
		return nil, invalid(identity.AuthFailureMalformed)
	}
	signature, err := encoding.DecodeString(parts[2])
	if err != nil {
		return nil, invalid(identity.AuthFailureMalformed)
	}
	return &parsedToken{
		signingInput: parts[0] + "." + parts[1],
		header:       header,
		claims:       claims,
		signature:    signature,
	}, nil
}

// verify checks the RFC 9068 profile of the token against the trusted key.
func (t *parsedToken) verify(key verificationKey) error {
	if !validAccessTokenType(t.header.Typ) {
		return invalid(identity.AuthFailureMalformed)
	}
	if _, ok := supportedAlgorithms[t.header.Alg]; !ok {
		return invalid(identity.AuthFailureUnsupportedAlgorithm)
	}
	digest := sha256.Sum256([]byte(t.signingInput))
	switch t.header.Alg {
	case "RS256":
		if key.rsa == nil {
			return invalid(identity.AuthFailureUnknownKey)
		}
		if err := rsa.VerifyPKCS1v15(key.rsa.public, crypto.SHA256, digest[:], t.signature); err != nil {
			return invalid(identity.AuthFailureInvalidSignature)
		}
	case "PS256":
		if key.rsa == nil {
			return invalid(identity.AuthFailureUnknownKey)
		}
		if err := rsa.VerifyPSS(key.rsa.public, crypto.SHA256, digest[:], t.signature, nil); err != nil {
			return invalid(identity.AuthFailureInvalidSignature)
		}
	case "ES256":
		if key.ec == nil {
			return invalid(identity.AuthFailureUnknownKey)
		}
		if len(t.signature) != 64 {
			return invalid(identity.AuthFailureInvalidSignature)
		}
		r := new(big.Int).SetBytes(t.signature[:32])
		s := new(big.Int).SetBytes(t.signature[32:])
		if !ecdsa.Verify(key.ec.public, digest[:], r, s) {
			return invalid(identity.AuthFailureInvalidSignature)
		}
	default:
		return invalid(identity.AuthFailureUnsupportedAlgorithm)
	}
	return nil
}

// rsaPublicKey wraps an RSA public key for verification keys.
type rsaPublicKey struct {
	public *rsa.PublicKey
}

// ecPublicKey wraps an ECDSA P-256 public key for verification keys.
type ecPublicKey struct {
	public *ecdsa.PublicKey
}

func parseRSAJWK(candidate jwk) (*rsaPublicKey, string, error) {
	if candidate.N == "" || candidate.E == "" {
		return nil, "", fmt.Errorf("incomplete RSA key")
	}
	modulus, err := encoding.DecodeString(candidate.N)
	if err != nil {
		return nil, "", fmt.Errorf("RSA modulus")
	}
	exponent, err := encoding.DecodeString(candidate.E)
	if err != nil {
		return nil, "", fmt.Errorf("RSA exponent")
	}
	public := &rsa.PublicKey{N: new(big.Int).SetBytes(modulus), E: int(new(big.Int).SetBytes(exponent).Int64())}
	if public.E < 3 || public.N.BitLen() < 2048 {
		return nil, "", fmt.Errorf("weak RSA key")
	}
	algorithm := candidate.Alg
	if algorithm != "RS256" && algorithm != "PS256" {
		algorithm = "RS256"
	}
	return &rsaPublicKey{public: public}, algorithm, nil
}

func parseECJWK(candidate jwk) (*ecPublicKey, error) {
	if candidate.Crv != "P-256" || candidate.X == "" || candidate.Y == "" {
		return nil, fmt.Errorf("unsupported EC key")
	}
	xBytes, err := encoding.DecodeString(candidate.X)
	if err != nil {
		return nil, fmt.Errorf("EC coordinate")
	}
	yBytes, err := encoding.DecodeString(candidate.Y)
	if err != nil {
		return nil, fmt.Errorf("EC coordinate")
	}
	curve := elliptic.P256()
	x := new(big.Int).SetBytes(xBytes)
	y := new(big.Int).SetBytes(yBytes)
	if !curve.IsOnCurve(x, y) {
		return nil, fmt.Errorf("EC point off curve")
	}
	return &ecPublicKey{public: &ecdsa.PublicKey{Curve: curve, X: x, Y: y}}, nil
}
