// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package transit

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/hashicorp/vault/sdk/framework"
	"github.com/hashicorp/vault/sdk/helper/keysutil"
	"github.com/hashicorp/vault/sdk/logical"
)

const (
	exportTypeEncryptionKey = "encryption-key"
	exportTypeSigningKey    = "signing-key"
	exportTypeHMACKey       = "hmac-key"
	exportTypePublicKey     = "public-key"
)

func (b *backend) pathExportKeys() *framework.Path {
	return &framework.Path{
		Pattern: "export/" + framework.GenericNameRegex("type") + "/" + framework.GenericNameRegex("name") + framework.OptionalParamRegex("version"),

		DisplayAttrs: &framework.DisplayAttributes{
			OperationPrefix: operationPrefixTransit,
			OperationVerb:   "export",
			OperationSuffix: "key|key-version",
		},

		Fields: map[string]*framework.FieldSchema{
			"type": {
				Type:        framework.TypeString,
				Description: "Type of key to export (encryption-key, signing-key, hmac-key, public-key)",
			},
			"name": {
				Type:        framework.TypeString,
				Description: "Name of the key",
			},
			"version": {
				Type:        framework.TypeString,
				Description: "Version of the key",
			},
		},

		Callbacks: map[logical.Operation]framework.OperationFunc{
			logical.ReadOperation: b.pathPolicyExportRead,
		},

		HelpSynopsis:    pathExportHelpSyn,
		HelpDescription: pathExportHelpDesc,
	}
}

func (b *backend) pathPolicyExportRead(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	exportType := d.Get("type").(string)
	name := d.Get("name").(string)
	version := d.Get("version").(string)

	switch exportType {
	case exportTypeEncryptionKey:
	case exportTypeSigningKey:
	case exportTypeHMACKey:
	case exportTypePublicKey:
	default:
		return logical.ErrorResponse(fmt.Sprintf("invalid export type: %s", exportType)), logical.ErrInvalidRequest
	}

	p, _, err := b.GetPolicy(ctx, keysutil.PolicyRequest{
		Storage: req.Storage,
		Name:    name,
	}, b.GetRandomReader())
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, nil
	}
	if !b.System().CachingDisabled() {
		p.Lock(false)
	}
	defer p.Unlock()

	if !p.Exportable && exportType != exportTypePublicKey {
		return logical.ErrorResponse("private key material is not exportable"), nil
	}

	switch exportType {
	case exportTypeEncryptionKey:
		if !p.Type.EncryptionSupported() {
			return logical.ErrorResponse("encryption not supported for the key"), logical.ErrInvalidRequest
		}
	case exportTypeSigningKey:
		if !p.Type.SigningSupported() {
			return logical.ErrorResponse("signing not supported for the key"), logical.ErrInvalidRequest
		}
	}

	retKeys := map[string]string{}
	switch version {
	case "":
		for k, v := range p.Keys {
			exportKey, err := getExportKey(p, &v, exportType)
			if err != nil {
				return nil, err
			}
			retKeys[k] = exportKey
		}

	default:
		var versionValue int
		if version == "latest" {
			versionValue = p.LatestVersion
		} else {
			version = strings.TrimPrefix(version, "v")
			versionValue, err = strconv.Atoi(version)
			if err != nil {
				return logical.ErrorResponse("invalid key version"), logical.ErrInvalidRequest
			}
		}

		if versionValue < p.MinDecryptionVersion {
			return logical.ErrorResponse("version for export is below minimum decryption version"), logical.ErrInvalidRequest
		}
		key, ok := p.Keys[strconv.Itoa(versionValue)]
		if !ok {
			return logical.ErrorResponse("version does not exist or cannot be found"), logical.ErrInvalidRequest
		}

		exportKey, err := getExportKey(p, &key, exportType)
		if err != nil {
			return nil, err
		}

		retKeys[strconv.Itoa(versionValue)] = exportKey
	}

	resp := &logical.Response{
		Data: map[string]interface{}{
			"name": p.Name,
			"type": p.Type.String(),
			"keys": retKeys,
		},
	}

	return resp, nil
}

func getExportKey(policy *keysutil.Policy, key *keysutil.KeyEntry, exportType string) (string, error) {
	if policy == nil {
		return "", errors.New("nil policy provided")
	}

	switch exportType {
	case exportTypeHMACKey:
		src := key.HMACKey
		if policy.Type == keysutil.KeyType_HMAC {
			src = key.Key
		}
		return strings.TrimSpace(base64.StdEncoding.EncodeToString(src)), nil

	case exportTypeEncryptionKey:
		switch policy.Type {
		case keysutil.KeyType_AES128_GCM96, keysutil.KeyType_AES256_GCM96, keysutil.KeyType_ChaCha20_Poly1305:
			return strings.TrimSpace(base64.StdEncoding.EncodeToString(key.Key)), nil

		case keysutil.KeyType_RSA2048, keysutil.KeyType_RSA3072, keysutil.KeyType_RSA4096:
			rsaKey, err := encodeRSAPrivateKey(key)
			if err != nil {
				return "", err
			}
			return rsaKey, nil
		}

	case exportTypeSigningKey:
		switch policy.Type {
		case keysutil.KeyType_ECDSA_P256, keysutil.KeyType_ECDSA_P384, keysutil.KeyType_ECDSA_P521:
			var curve elliptic.Curve
			switch policy.Type {
			case keysutil.KeyType_ECDSA_P384:
				curve = elliptic.P384()
			case keysutil.KeyType_ECDSA_P521:
				curve = elliptic.P521()
			default:
				curve = elliptic.P256()
			}
			ecKey, err := keyEntryToECPrivateKey(key, curve)
			if err != nil {
				return "", err
			}
			return ecKey, nil

		case keysutil.KeyType_ED25519:
			if len(key.Key) == 0 {
				return "", nil
			}

			return strings.TrimSpace(base64.StdEncoding.EncodeToString(key.Key)), nil

		case keysutil.KeyType_RSA2048, keysutil.KeyType_RSA3072, keysutil.KeyType_RSA4096:
			rsaKey, err := encodeRSAPrivateKey(key)
			if err != nil {
				return "", err
			}
			return rsaKey, nil
		}
	case exportTypePublicKey:
		switch policy.Type {
		case keysutil.KeyType_ECDSA_P256, keysutil.KeyType_ECDSA_P384, keysutil.KeyType_ECDSA_P521:
			var curve elliptic.Curve
			switch policy.Type {
			case keysutil.KeyType_ECDSA_P384:
				curve = elliptic.P384()
			case keysutil.KeyType_ECDSA_P521:
				curve = elliptic.P521()
			default:
				curve = elliptic.P256()
			}
			ecKey, err := keyEntryToECPublicKey(key, curve)
			if err != nil {
				return "", err
			}
			return ecKey, nil

		case keysutil.KeyType_ED25519:
			return strings.TrimSpace(key.FormattedPublicKey), nil

		case keysutil.KeyType_RSA2048, keysutil.KeyType_RSA3072, keysutil.KeyType_RSA4096:
			rsaKey, err := encodeRSAPublicKey(key)
			if err != nil {
				return "", err
			}
			return rsaKey, nil
		}
	}

	return "", fmt.Errorf("unknown key type %v for export type %v", policy.Type, exportType)
}

func encodeRSAPrivateKey(key *keysutil.KeyEntry) (string, error) {
	if key == nil {
		return "", errors.New("nil KeyEntry provided")
	}

	if key.IsPrivateKeyMissing() {
		return "", nil
	}

	// When encoding PKCS1, the PEM header should be `RSA PRIVATE KEY`. When Go
	// has PKCS8 encoding support, we may want to change this.
	blockType := "RSA PRIVATE KEY"
	derBytes := x509.MarshalPKCS1PrivateKey(key.RSAKey)
	pemBlock := pem.Block{
		Type:  blockType,
		Bytes: derBytes,
	}

	pemBytes := pem.EncodeToMemory(&pemBlock)
	return string(pemBytes), nil
}

func encodeRSAPublicKey(key *keysutil.KeyEntry) (string, error) {
	if key == nil {
		return "", errors.New("nil KeyEntry provided")
	}

	blockType := "RSA PUBLIC KEY"
	derBytes, err := x509.MarshalPKIXPublicKey(key.RSAPublicKey)
	if err != nil {
		return "", err
	}

	pemBlock := pem.Block{
		Type:  blockType,
		Bytes: derBytes,
	}

	pemBytes := pem.EncodeToMemory(&pemBlock)
	return string(pemBytes), nil
}

func keyEntryToECPrivateKey(k *keysutil.KeyEntry, curve elliptic.Curve) (string, error) {
	if k == nil {
		return "", errors.New("nil KeyEntry provided")
	}

	if k.IsPrivateKeyMissing() {
		return "", nil
	}

	pubKey := ecdsa.PublicKey{
		Curve: curve,
		X:     k.EC_X,
		Y:     k.EC_Y,
	}

	blockType := "EC PRIVATE KEY"
	privKey := &ecdsa.PrivateKey{
		PublicKey: pubKey,
		D:         k.EC_D,
	}
	derBytes, err := x509.MarshalECPrivateKey(privKey)
	if err != nil {
		return "", err
	}

	pemBlock := pem.Block{
		Type:  blockType,
		Bytes: derBytes,
	}

	return strings.TrimSpace(string(pem.EncodeToMemory(&pemBlock))), nil
}

func keyEntryToECPublicKey(k *keysutil.KeyEntry, curve elliptic.Curve) (string, error) {
	if k == nil {
		return "", errors.New("nil KeyEntry provided")
	}

	pubKey := ecdsa.PublicKey{
		Curve: curve,
		X:     k.EC_X,
		Y:     k.EC_Y,
	}

	blockType := "PUBLIC KEY"
	derBytes, err := x509.MarshalPKIXPublicKey(&pubKey)
	if err != nil {
		return "", err
	}

	pemBlock := pem.Block{
		Type:  blockType,
		Bytes: derBytes,
	}

	return strings.TrimSpace(string(pem.EncodeToMemory(&pemBlock))), nil
}

const pathExportHelpSyn = `Export named encryption or signing key`

const pathExportHelpDesc = `
This path is used to export the named keys that are configured as
exportable.
`
