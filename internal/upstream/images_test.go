package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/BegoniaHe/wokbuddy-demon/internal/auth"
)

// TestEditImagePostsJSONToEditsPath EditImage 打 /v2/images/edits，body 原样透传
// （官方 CLI editImage 是 JSON 形态，不是 multipart）。
func TestEditImagePostsJSONToEditsPath(t *testing.T) {
	t.Parallel()

	var gotPath, gotContentType, gotAccept string

	var gotBody []byte

	client := testClient(func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		gotAccept = r.Header.Get("Accept")
		gotBody, _ = io.ReadAll(r.Body)

		return jsonResp(200, `{"created":1,"data":[{"url":"https://img.example/e.png"}],"usage":{"total_tokens":5}}`), nil
	})

	body := []byte(`{"model":"image-to-image","prompt":"make it blue","size":"1024x1024",` +
		`"image":["data:image/png;base64,AAA"],"input_fidelity":"high"}`)

	status, raw, err := client.EditImage(
		context.Background(), &auth.Auth{AccessToken: "at", UID: "u1"}, body, "", ChatMeta{},
	)
	if err != nil {
		t.Fatalf("edit image: %v", err)
	}

	if status != 200 || len(raw) == 0 {
		t.Fatalf("status=%d raw=%s", status, raw)
	}

	if gotPath != ImageEditsPath {
		t.Errorf("path=%s want %s", gotPath, ImageEditsPath)
	}

	if gotContentType != "application/json" || gotAccept != "application/json" {
		t.Errorf("content-type=%q accept=%q want application/json", gotContentType, gotAccept)
	}

	// 上游字段（含 image 数组与 input_fidelity）原样到达，网关不做改写。
	if string(gotBody) != string(body) {
		t.Errorf("body=%s want passthrough %s", gotBody, body)
	}
}

// TestGenerateImageStaysOnGenerationsPath 出图路径不因新增 edits 而漂移。
func TestGenerateImageStaysOnGenerationsPath(t *testing.T) {
	t.Parallel()

	var gotPath string

	client := testClient(func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path

		return jsonResp(200, `{"created":1,"data":[]}`), nil
	})

	if _, _, err := client.GenerateImage(
		context.Background(), &auth.Auth{AccessToken: "at", UID: "u1"}, []byte(`{"model":"m"}`), "", ChatMeta{},
	); err != nil {
		t.Fatalf("generate image: %v", err)
	}

	if gotPath != ImageGenerationsPath {
		t.Errorf("path=%s want %s", gotPath, ImageGenerationsPath)
	}
}

// TestEditImageUpstreamErrorCarriesStatus 上游 4xx 归一为 *Error 且保留 body 供透传。
func TestEditImageUpstreamErrorCarriesStatus(t *testing.T) {
	t.Parallel()

	client := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(400, `{"code":400,"msg":"image is required"}`), nil
	})

	status, raw, err := client.EditImage(
		context.Background(), &auth.Auth{AccessToken: "at", UID: "u1"}, []byte(`{"model":"m"}`), "", ChatMeta{},
	)
	if err == nil {
		t.Fatal("want error")
	}

	var upstreamErr *Error
	if !errors.As(err, &upstreamErr) {
		t.Fatalf("err=%T want *Error", err)
	}

	if status != 400 || upstreamErr.Status != 400 || len(raw) == 0 {
		t.Errorf("status=%d upstreamErr.Status=%d raw=%s", status, upstreamErr.Status, raw)
	}
}
