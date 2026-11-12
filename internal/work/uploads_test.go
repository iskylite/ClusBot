package work

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestUploadPartsKeepUTF8AndPersistSelectedSnapshot(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := filepath.Join(dir, "test.db")
	s, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	root := filepath.Join(dir, "uploads")
	if err = s.OpenUploads(root); err != nil {
		t.Fatal(err)
	}
	text := strings.Repeat("a", uploadStride-1) + "界面第二段"
	first, err := s.UploadMaterial(ctx, "会议记录.md", []byte(text))
	if err != nil {
		t.Fatal(err)
	}
	if first.Upload == nil || first.Upload.Parts != 2 || first.Size != uploadStride-1 {
		t.Fatalf("incorrect first part: %+v", first)
	}
	second, err := s.UploadMaterialPart(ctx, first.Upload.ID, 2)
	if err != nil || second.Size > maxMaterialBytes || second.Upload == nil || second.Upload.Digest != first.Upload.Digest {
		t.Fatalf("incorrect second part: %+v, %v", second, err)
	}
	if _, err = s.UploadMaterialPart(ctx, first.Upload.ID, 3); !errors.Is(err, ErrUploadPart) {
		t.Fatalf("invalid part: %v", err)
	}
	r, err := s.Submit(ctx, "upload-selection", "读取片段", "", "", true, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.OpenUploads(root); err != nil {
		t.Fatal(err)
	}
	input, err := s.modelMessages(ctx, r.ConversationID)
	if err != nil || len(input) != 1 || len(input[0].Materials) != 1 {
		t.Fatalf("missing attachment: %+v, %v", input, err)
	}
	m := input[0].Materials[0]
	if m.Content != "界面第二段" || !utf8.ValidString(m.Content) || m.Upload == nil || m.Upload.Part != 2 {
		t.Fatalf("wrong snapshot: %+v", m)
	}
	// Editing the private original cannot change an attached snapshot.
	if err = os.WriteFile(filepath.Join(root, first.Upload.ID+".txt"), []byte("modified"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = s.UploadMaterialPart(ctx, first.Upload.ID, 1); !errors.Is(err, ErrUploadChanged) {
		t.Fatalf("modified original accepted: %v", err)
	}
	input, err = s.modelMessages(ctx, r.ConversationID)
	if err != nil || input[0].Materials[0].Content != "界面第二段" {
		t.Fatalf("saved snapshot was lost: %v", err)
	}
}

func TestUploadRejectsInvalidFiles(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, fixture := range []struct {
		name string
		data []byte
	}{
		{"../escape.md", []byte("text")}, {"image.png", []byte("text")}, {"bad.md", []byte{0xff}},
		{"null.txt", []byte{0}}, {"empty.txt", nil},
	} {
		if _, err = s.UploadMaterial(ctx, fixture.name, fixture.data); !errors.Is(err, ErrUpload) {
			t.Fatalf("%s accepted: %v", fixture.name, err)
		}
	}
	if _, err = s.UploadMaterial(ctx, "large.md", make([]byte, MaxUploadBytes+1)); !errors.Is(err, ErrUploadSize) {
		t.Fatalf("oversized input accepted: %v", err)
	}
}
