package youtubemock

import (
	"bytes"
	"os"
	"slices"
	"testing"

	"gopkg.in/yaml.v3"
)

const schemaStringType = "string"

type uploadContract struct {
	OpenAPI string `yaml:"openapi"`
	Paths   map[string]struct {
		Post *uploadOperation `yaml:"post"`
	} `yaml:"paths"`
}

type uploadOperation struct {
	OperationID string `yaml:"operationId"`
	Servers     []struct {
		URL string `yaml:"url"`
	} `yaml:"servers"`
	Parameters []struct {
		Name     string `yaml:"name"`
		In       string `yaml:"in"`
		Required bool   `yaml:"required"`
		Schema   struct {
			Type  string `yaml:"type"`
			Const string `yaml:"const"`
		} `yaml:"schema"`
	} `yaml:"parameters"`
	RequestBody struct {
		Required bool `yaml:"required"`
		Content  map[string]struct {
			Schema struct {
				Type        string `yaml:"type"`
				MinItems    int    `yaml:"minItems"`
				MaxItems    int    `yaml:"maxItems"`
				PrefixItems []struct {
					Title     string `yaml:"title"`
					Ref       string `yaml:"$ref"`
					Type      string `yaml:"type"`
					Format    string `yaml:"format"`
					MaxLength int64  `yaml:"maxLength"`
				} `yaml:"prefixItems"`
			} `yaml:"schema"`
			PrefixEncoding []struct {
				ContentType string `yaml:"contentType"`
			} `yaml:"prefixEncoding"`
		} `yaml:"content"`
	} `yaml:"requestBody"`
	Responses map[string]any        `yaml:"responses"`
	Security  []map[string][]string `yaml:"security"`
}

func TestVideosInsertContractIsSingleShotMultipart(t *testing.T) {
	t.Parallel()

	specBytes, err := os.ReadFile("youtube.yaml")
	if err != nil {
		t.Fatalf("read YouTube OpenAPI contract: %v", err)
	}

	var spec uploadContract
	if err := yaml.Unmarshal(specBytes, &spec); err != nil {
		t.Fatalf("decode YouTube OpenAPI contract: %v", err)
	}
	for _, artifact := range [][]byte{
		[]byte("#/components/parameters/upload_protocol"),
		[]byte("#/components/parameters/uploadType"),
	} {
		if bytes.Contains(specBytes, artifact) {
			t.Errorf("converted discovery artifact %q remains in unrelated operations", artifact)
		}
	}

	if spec.OpenAPI != "3.2.0" {
		t.Errorf("OpenAPI version = %q, want 3.2.0", spec.OpenAPI)
	}
	if legacy := spec.Paths["/youtube/v3/videos"].Post; legacy != nil {
		t.Errorf("POST /youtube/v3/videos still declares videos.insert")
	}

	insert := spec.Paths["/upload/youtube/v3/videos"].Post
	if insert == nil {
		t.Fatal("POST /upload/youtube/v3/videos is not declared")
	}
	assertUploadOperation(t, insert)

	assertUploadParameters(t, insert)

	assertUploadBodyAndResponses(t, insert)

	wantScopes := []string{
		"https://www.googleapis.com/auth/youtube",
		"https://www.googleapis.com/auth/youtube.force-ssl",
		"https://www.googleapis.com/auth/youtube.upload",
		"https://www.googleapis.com/auth/youtubepartner",
	}
	var scopes []string
	for _, requirement := range insert.Security {
		scopes = append(scopes, requirement["Oauth2"]...)
	}
	if !slices.Equal(scopes, wantScopes) {
		t.Errorf("Oauth2 scopes = %#v, want %#v", scopes, wantScopes)
	}
}

func assertUploadParameters(t *testing.T, insert *uploadOperation) {
	t.Helper()
	const queryLocation = "query"
	parameters := make(map[string]struct {
		In       string
		Required bool
		Type     string
		Const    string
	}, len(insert.Parameters))
	for _, parameter := range insert.Parameters {
		parameters[parameter.Name] = struct {
			In       string
			Required bool
			Type     string
			Const    string
		}{parameter.In, parameter.Required, parameter.Schema.Type, parameter.Schema.Const}
	}
	for name, want := range map[string]struct {
		In       string
		Required bool
		Type     string
		Const    string
	}{
		"part":       {In: queryLocation, Required: true, Type: schemaStringType},
		"uploadType": {In: queryLocation, Required: true, Type: schemaStringType, Const: "multipart"},
	} {
		if got := parameters[name]; got != want {
			t.Errorf("%s parameter = %#v, want %#v", name, got, want)
		}
	}
	for _, optional := range []string{"notifySubscribers", "onBehalfOfContentOwner", "onBehalfOfContentOwnerChannel"} {
		if got, ok := parameters[optional]; !ok || got.In != queryLocation || got.Required {
			t.Errorf("%s parameter = %#v, want optional query parameter", optional, got)
		}
	}
	if len(parameters) != 5 {
		t.Errorf("videos.insert has %d parameters, want exactly 5", len(parameters))
	}
}

func assertUploadBodyAndResponses(t *testing.T, insert *uploadOperation) {
	t.Helper()
	body, ok := insert.RequestBody.Content["multipart/related"]
	if !insert.RequestBody.Required || !ok {
		t.Fatalf(
			"required multipart/related request body = required %t, present %t",
			insert.RequestBody.Required,
			ok,
		)
	}
	if body.Schema.Type != "array" || body.Schema.MinItems != 2 || body.Schema.MaxItems != 2 ||
		len(body.Schema.PrefixItems) != 2 {
		t.Fatalf("ordered multipart schema = %#v, want exactly two positional parts", body.Schema)
	}
	assertUploadPartSchemas(t, insert)

	if len(body.PrefixEncoding) != 2 || body.PrefixEncoding[0].ContentType != "application/json" ||
		body.PrefixEncoding[1].ContentType != "video/*, application/octet-stream" {
		t.Errorf(
			"prefixEncoding = %#v, want JSON metadata then supported video media",
			body.PrefixEncoding,
		)
	}
	for _, status := range []string{"200", "400", "403"} {
		if _, ok := insert.Responses[status]; !ok {
			t.Errorf("videos.insert response %s is not declared", status)
		}
	}
}

func assertUploadOperation(t *testing.T, insert *uploadOperation) {
	t.Helper()
	if insert.OperationID != "youtube.videos.insert" {
		t.Errorf("operationId = %q, want youtube.videos.insert", insert.OperationID)
	}
	if len(insert.Servers) != 1 || insert.Servers[0].URL != "https://www.googleapis.com" {
		t.Errorf("operation servers = %#v, want https://www.googleapis.com", insert.Servers)
	}
}

func assertUploadPartSchemas(t *testing.T, insert *uploadOperation) {
	t.Helper()
	body := insert.RequestBody.Content["multipart/related"]
	metadata, media := body.Schema.PrefixItems[0], body.Schema.PrefixItems[1]
	if metadata.Title != "metadata" || metadata.Ref != "#/components/schemas/Video" {
		t.Errorf("metadata part = %#v, want Video schema first", metadata)
	}
	if media.Title != "media" || media.Type != schemaStringType || media.Format != "binary" ||
		media.MaxLength != 274877906944 {
		t.Errorf("media part = %#v, want binary media up to 256 GiB second", media)
	}
}
