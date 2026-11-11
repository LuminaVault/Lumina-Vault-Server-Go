package hermescaps

import (
	"encoding/json"
	"reflect"
	"testing"
)

// Ported from AppTests/Hermes/HermesCapabilitiesParseTests.swift.

func TestParseFlagsFeaturesMap(t *testing.T) {
	body := []byte(`{"features":{
		"chat_completions":true,"responses_api":true,
		"session_resources":true,"skills_api":true,
		"admin_config_rw":false,"jobs_admin":false,"memory_write_api":false,
		"multimodal_ingestion":true,"ingestion_remote_source_url":true,
		"ingestion_supported_mime_types":["application/pdf","image/*"],
		"ingestion_max_source_bytes":104857600
	}}`)
	f := ParseFlags(body)
	if f == nil {
		t.Fatal("nil flags")
	}
	if !f.Sessions || !f.Skills || f.Jobs || !f.MultimodalIngestion || !f.RemoteSourceURL {
		t.Fatalf("flags = %+v", f)
	}
	if f.MimeTypes == nil || !reflect.DeepEqual(*f.MimeTypes, []string{"application/pdf", "image/*"}) {
		t.Fatalf("mime types = %v", f.MimeTypes)
	}
	if f.MaxSourceBytes == nil || *f.MaxSourceBytes != 104_857_600 {
		t.Fatalf("max bytes = %v", f.MaxSourceBytes)
	}
}

func TestParseFlagsFlatMap(t *testing.T) {
	f := ParseFlags([]byte(`{"sessions":true,"skills_api":true,"jobs":true}`))
	if f == nil || !f.Sessions || !f.Skills || !f.Jobs {
		t.Fatalf("flags = %+v", f)
	}
}

func TestParseFlagsUnknownFalse(t *testing.T) {
	f := ParseFlags([]byte(`{"features":{}}`))
	if f == nil || !reflect.DeepEqual(*f, Flags{}) {
		t.Fatalf("flags = %+v", f)
	}
}

func TestParseFlagsGarbageNil(t *testing.T) {
	if f := ParseFlags([]byte("nope")); f != nil {
		t.Fatalf("flags = %+v", f)
	}
}

func TestManagedDefaultAllManaged(t *testing.T) {
	c := ManagedDefault()
	if c.IsUserOverride {
		t.Fatal("isUserOverride")
	}
	for _, d := range []Availability{c.Chat, c.Sessions, c.Jobs, c.Skills, c.Soul, c.Gateways, c.Memory, c.Providers, *c.MultimodalIngestion} {
		if d != Managed {
			t.Fatalf("domain = %q", d)
		}
	}
}

// The cached row is decoded back on the next read; the round trip must hold.
func TestCapabilitiesJSONRoundTrip(t *testing.T) {
	c := ManagedDefault()
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var back Capabilities
	if err := decodeCapabilities(data, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c, back) {
		t.Fatalf("round trip:\n%+v\n%+v", c, back)
	}
}
