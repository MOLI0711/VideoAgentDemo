package api

import (
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/MOLI0711/MOLIVedio/internal/domain"
	"github.com/MOLI0711/MOLIVedio/internal/storage"
)

func TestVerifyPartsAcceptsStoredManifest(t *testing.T) {
	const partSize int64 = 8 << 20
	cases := []struct {
		name   string
		size   int64
		sent   []partDTO
		actual []storage.Part
	}{
		{"short single part", 3, []partDTO{{Number: 1, ETag: "abc"}}, []storage.Part{{Number: 1, ETag: `"abc"`, Size: 3}}},
		{"exact boundary", partSize, []partDTO{{Number: 1, ETag: `"abc"`}}, []storage.Part{{Number: 1, ETag: `"abc"`, Size: partSize}}},
		{"unordered short tail", partSize + 3, []partDTO{{Number: 2, ETag: "tail"}, {Number: 1, ETag: "head"}}, []storage.Part{{Number: 2, ETag: "tail", Size: 3}, {Number: 1, ETag: "head", Size: partSize}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := verifyParts(tc.size, partSize, tc.sent, tc.actual); err != nil {
				t.Fatalf("valid manifest rejected: %v", err)
			}
		})
	}
}

func TestVerifyPartsRejectsMismatchedManifest(t *testing.T) {
	const partSize int64 = 8 << 20
	baseSent := []partDTO{{Number: 1, ETag: "head"}, {Number: 2, ETag: "tail"}}
	baseActual := []storage.Part{{Number: 1, ETag: "head", Size: partSize}, {Number: 2, ETag: "tail", Size: 3}}
	cases := []struct {
		name   string
		change func(*[]partDTO, *[]storage.Part)
		code   string
	}{
		{"missing client part", func(s *[]partDTO, _ *[]storage.Part) { *s = (*s)[:1] }, "UPLOAD_INCOMPLETE"},
		{"missing stored part", func(_ *[]partDTO, a *[]storage.Part) { *a = (*a)[:1] }, "UPLOAD_INCOMPLETE"},
		{"extra stored part", func(_ *[]partDTO, a *[]storage.Part) {
			*a = append(*a, storage.Part{Number: 3, ETag: "extra", Size: 3})
		}, "UPLOAD_INCOMPLETE"},
		{"client duplicate", func(s *[]partDTO, _ *[]storage.Part) { (*s)[1].Number = 1 }, "UPLOAD_PART_MISMATCH"},
		{"stored duplicate", func(_ *[]partDTO, a *[]storage.Part) { (*a)[1].Number = 1 }, "UPLOAD_PART_MISMATCH"},
		{"number gap", func(_ *[]partDTO, a *[]storage.Part) { (*a)[1].Number = 3 }, "UPLOAD_PART_MISMATCH"},
		{"etag mismatch", func(s *[]partDTO, _ *[]storage.Part) { (*s)[1].ETag = "untrusted" }, "UPLOAD_PART_MISMATCH"},
		{"empty etag", func(s *[]partDTO, a *[]storage.Part) { (*s)[1].ETag = ""; (*a)[1].ETag = "" }, "UPLOAD_PART_MISMATCH"},
		{"short middle", func(_ *[]partDTO, a *[]storage.Part) { (*a)[0].Size = partSize - 1 }, "UPLOAD_PART_MISMATCH"},
		{"wrong tail", func(_ *[]partDTO, a *[]storage.Part) { (*a)[1].Size = 4 }, "UPLOAD_PART_MISMATCH"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sent := append([]partDTO{}, baseSent...)
			actual := append([]storage.Part{}, baseActual...)
			tc.change(&sent, &actual)
			err := verifyParts(partSize+3, partSize, sent, actual)
			var failure *failure
			if !errors.As(err, &failure) || failure.Code != tc.code || failure.Status != 409 {
				t.Fatalf("want 409 %s, got %v", tc.code, err)
			}
		})
	}
}

func TestVerifyPartsRejectsInvalidSizes(t *testing.T) {
	for _, tc := range []struct{ size, partSize int64 }{{0, 8 << 20}, {-1, 8 << 20}, {100, 0}, {100, -1}} {
		t.Run("invalid sizes", func(t *testing.T) {
			defer func() {
				if recovered := recover(); recovered != nil {
					t.Fatalf("must return an error, not panic: %v", recovered)
				}
			}()
			if err := verifyParts(tc.size, tc.partSize, nil, nil); err == nil {
				t.Fatal("invalid upload sizes accepted")
			}
		})
	}
}

func TestEventCursor(t *testing.T) {
	cases := []struct {
		name, query, header string
		want                int64
		invalid             bool
	}{
		{"initial", "", "", 0, false},
		{"query", "after_seq=7", "", 7, false},
		{"header wins", "after_seq=7", "12", 12, false},
		{"zero header wins", "after_seq=7", "0", 0, false},
		{"header independent of bad query", "after_seq=bad", "2", 2, false},
		{"negative query", "after_seq=-1", "", 0, true},
		{"negative header", "after_seq=1", "-1", 0, true},
		{"malformed header rejected", "after_seq=1", "bad", 0, true},
		{"fraction", "after_seq=1.5", "", 0, true},
		{"overflow", "after_seq=9223372036854775808", "", 0, true},
		{"max int", "after_seq=9223372036854775807", "", 9223372036854775807, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/v1/runs/id/events?"+tc.query, nil)
			if tc.header != "" {
				r.Header.Set("Last-Event-ID", tc.header)
			}
			got, err := eventCursor(r)
			if tc.invalid {
				if !errors.Is(err, domain.ErrInvalid) {
					t.Fatalf("want invalid cursor, got %d %v", got, err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("want %d, got %d %v", tc.want, got, err)
			}
		})
	}
}
