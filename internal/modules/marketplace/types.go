package marketplace

import (
	"encoding/json"
	"lumina/internal/wire"
)

// Request bodies, in Swift's declaration order (Bind reports the first failing
// key in field order; the generated models are alphabetical).

type ratingRequest struct {
	Rating int     `json:"rating"`
	Body   *string `json:"body"`
}

type installRequest struct {
	VersionID          wire.UUID          `json:"versionId"`
	GrantedPermissions []PluginPermission `json:"grantedPermissions"`
	Config             map[string]string  `json:"config"`
}

type upgradeRequest struct {
	FromVersionID      wire.UUID          `json:"fromVersionId"`
	ToVersionID        wire.UUID          `json:"toVersionId"`
	GrantedPermissions []PluginPermission `json:"grantedPermissions"`
	Config             map[string]string  `json:"config"`
}

type toolRunRequest struct {
	Input map[string]string `json:"input"`
}

type publisherApplicationRequest struct {
	Handle      string  `json:"handle"`
	DisplayName string  `json:"displayName"`
	Bio         *string `json:"bio"`
	WebsiteURL  *string `json:"websiteURL"`
}

type listingCreateRequest struct {
	Slug        string         `json:"slug"`
	Name        string         `json:"name"`
	Summary     string         `json:"summary"`
	Description string         `json:"description"`
	Category    PluginCategory `json:"category"`
	IconURL     *string        `json:"iconURL"`
	Screenshots *[]string      `json:"screenshots"`
}

// configFieldInput is PluginConfigField in Swift's order; every key but
// placeholder is required on input.
type configFieldInput struct {
	Key         string                `json:"key"`
	Label       string                `json:"label"`
	Placeholder *string               `json:"placeholder"`
	Kind        PluginConfigFieldKind `json:"kind"`
	IsRequired  bool                  `json:"isRequired"`
}

type versionCreateRequest struct {
	Version           string                 `json:"version"`
	RuntimeKind       MarketplaceRuntimeKind `json:"runtimeKind"`
	Permissions       []PluginPermission     `json:"permissions"`
	NetworkHosts      *[]string              `json:"networkHosts"`
	ConfigFields      *[]configFieldInput    `json:"configFields"`
	Changelog         *string                `json:"changelog"`
	ArtifactKey       *string                `json:"artifactKey"`
	ArtifactSHA256    *string                `json:"artifactSHA256"`
	ArtifactSignature *string                `json:"artifactSignature"`
	ManifestJSON      *string                `json:"manifestJSON"`
}

// artifactUploadRequest: the spec says format: byte, but Swift takes a String
// and decides what is valid base64 itself.
type artifactUploadRequest struct {
	FileName    string `json:"fileName"`
	BytesBase64 string `json:"bytesBase64"`
}

type moderationRequest struct {
	Approved bool    `json:"approved"`
	Note     *string `json:"note"`
}

type revokeRequest struct {
	Reason string `json:"reason"`
}

// Swift's enums here have synthesized decoders, so an unknown raw value is a
// data-corrupted error naming the type.

func enumString(b []byte, typeName string, valid func(string) bool) (string, error) {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return "", err
	}
	if !valid(s) {
		return "", &wire.CorruptedError{Message: "Cannot initialize " + typeName + " from invalid String value " + s}
	}
	return s, nil
}

func (p *PluginPermission) UnmarshalJSON(b []byte) error {
	s, err := enumString(b, "PluginPermission", func(s string) bool { return PluginPermission(s).Valid() })
	if err != nil {
		return err
	}
	*p = PluginPermission(s)
	return nil
}

func (p *PluginCategory) UnmarshalJSON(b []byte) error {
	s, err := enumString(b, "PluginCategory", func(s string) bool { return PluginCategory(s).Valid() })
	if err != nil {
		return err
	}
	*p = PluginCategory(s)
	return nil
}

func (k *MarketplaceRuntimeKind) UnmarshalJSON(b []byte) error {
	s, err := enumString(b, "MarketplaceRuntimeKind", func(s string) bool { return MarketplaceRuntimeKind(s).Valid() })
	if err != nil {
		return err
	}
	*k = MarketplaceRuntimeKind(s)
	return nil
}

func (k *PluginConfigFieldKind) UnmarshalJSON(b []byte) error {
	s, err := enumString(b, "PluginConfigFieldKind", func(s string) bool { return PluginConfigFieldKind(s).Valid() })
	if err != nil {
		return err
	}
	*k = PluginConfigFieldKind(s)
	return nil
}
