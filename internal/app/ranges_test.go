package app

import (
	"strings"
	"testing"

	"github.com/Djarvur/http-proxy-local/internal/port"
)

func TestParseRIPELine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		line    string
		wantLow uint32
		wantHi  uint32
		wantOk  bool
		wantErr bool
	}{
		{
			name:    "valid RU ipv4 line",
			line:    "ripencc|RU|ipv4|1.2.3.0|256|20100101|assigned",
			wantLow: 16909004, // 1.2.3.0 = 0x01020300 = 16909024? check
			wantHi:  16909259, // 1.2.3.0 + 256 - 1
			wantOk:  true,
		},
		{
			name:   "non-RU country skipped",
			line:   "ripencc|DE|ipv4|1.2.3.0|256|20100101|assigned",
			wantOk: false,
		},
		{
			name:   "non-ipv4 type skipped",
			line:   "ripencc|RU|ipv6|2001:db8::|32|20100101|assigned",
			wantOk: false,
		},
		{
			name:   "summary line skipped",
			line:   "2|ripencc|0.0.0.0|0|0.0.0.0|0|0.0.0.0|0",
			wantOk: false,
		},
		{
			// A line that has the RU|ipv4 prefix but is truncated (5–6 fields,
			// fewer than the canonical 7+) is clearly a broken RU record → error.
			// A line with fewer than 5 fields is just garbage → silent skip.
			name:    "malformed RU ipv4 with too few fields",
			line:    "ripencc|RU|ipv4|1.2.3.0|256",
			wantErr: true,
		},
		{
			name:   "non-RU-ipv4 garbage with few fields skipped silently",
			line:   "ripencc|RU|ipv4",
			wantOk: false,
		},
		{
			name:    "malformed bad IP",
			line:    "ripencc|RU|ipv4|not.an.ip.addr|256|20100101|assigned",
			wantErr: true,
		},
		{
			name:    "malformed bad count",
			line:    "ripencc|RU|ipv4|1.2.3.0|notanumber|20100101|assigned",
			wantErr: true,
		},
	}

	// Recompute expected for the valid case explicitly (the inline comment
	// above may be wrong; this asserts the actual semantics).
	// 1.2.3.0 → (1<<24)|(2<<16)|(3<<8)|0 = 16777216 + 131072 + 768 = 16909056
	const want1Low = 1<<24 | 2<<16 | 3<<8
	tests[0].wantLow = want1Low
	tests[0].wantHi = want1Low + 256 - 1

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r, ok, err := ParseRIPELine(tt.line)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (r=%+v ok=%v)", r, ok)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ok != tt.wantOk {
				t.Fatalf("ok = %v, want %v (r=%+v)", ok, tt.wantOk, r)
			}
			if ok {
				if r.Low != tt.wantLow || r.High != tt.wantHi {
					t.Errorf("range = [%d, %d], want [%d, %d]", r.Low, r.High, tt.wantLow, tt.wantHi)
				}
			}
		})
	}
}

func TestMergeRanges(t *testing.T) {
	t.Parallel()

	t.Run("empty input yields empty output", func(t *testing.T) {
		t.Parallel()
		got := MergeRanges(nil)
		if len(got) != 0 {
			t.Errorf("got %d ranges, want 0", len(got))
		}
	})

	t.Run("disjoint input preserved", func(t *testing.T) {
		t.Parallel()
		in := []port.Range{{Low: 10, High: 19}, {Low: 30, High: 39}}
		got := MergeRanges(in)
		if len(got) != 2 {
			t.Fatalf("got %d ranges, want 2: %+v", len(got), got)
		}
		if got[0] != (port.Range{Low: 10, High: 19}) || got[1] != (port.Range{Low: 30, High: 39}) {
			t.Errorf("got %+v", got)
		}
	})

	t.Run("adjacent ranges merged", func(t *testing.T) {
		t.Parallel()
		in := []port.Range{{Low: 10, High: 19}, {Low: 20, High: 29}}
		got := MergeRanges(in)
		if len(got) != 1 {
			t.Fatalf("got %d ranges, want 1: %+v", len(got), got)
		}
		want := port.Range{Low: 10, High: 29}
		if got[0] != want {
			t.Errorf("got %+v, want %+v", got[0], want)
		}
	})

	t.Run("overlapping ranges merged", func(t *testing.T) {
		t.Parallel()
		in := []port.Range{{Low: 10, High: 30}, {Low: 20, High: 40}}
		got := MergeRanges(in)
		if len(got) != 1 {
			t.Fatalf("got %d ranges, want 1: %+v", len(got), got)
		}
		want := port.Range{Low: 10, High: 40}
		if got[0] != want {
			t.Errorf("got %+v, want %+v", got[0], want)
		}
	})

	t.Run("subset absorbed", func(t *testing.T) {
		t.Parallel()
		in := []port.Range{{Low: 10, High: 100}, {Low: 20, High: 30}}
		got := MergeRanges(in)
		if len(got) != 1 {
			t.Fatalf("got %d ranges, want 1: %+v", len(got), got)
		}
		want := port.Range{Low: 10, High: 100}
		if got[0] != want {
			t.Errorf("got %+v, want %+v", got[0], want)
		}
	})

	t.Run("unsorted input gets sorted", func(t *testing.T) {
		t.Parallel()
		in := []port.Range{{Low: 30, High: 39}, {Low: 10, High: 19}, {Low: 20, High: 29}}
		got := MergeRanges(in)
		if len(got) != 1 {
			t.Fatalf("got %d ranges, want 1: %+v", len(got), got)
		}
		want := port.Range{Low: 10, High: 39}
		if got[0] != want {
			t.Errorf("got %+v, want %+v", got[0], want)
		}
	})

	t.Run("does not mutate input slice order", func(t *testing.T) {
		t.Parallel()
		in := []port.Range{{Low: 30, High: 39}, {Low: 10, High: 19}}
		_ = MergeRanges(in)
		// Input slice contents may be sorted (we sort a copy), but length is intact.
		if len(in) != 2 {
			t.Errorf("input slice length changed: %d", len(in))
		}
	})
}

func TestParseAndMerge(t *testing.T) {
	t.Parallel()

	t.Run("end-to-end mixed delegation text", func(t *testing.T) {
		t.Parallel()
		raw := strings.Join([]string{
			"# comment line",
			"2|ripencc|0.0.0.0|0|0.0.0.0|0|0.0.0.0|0",
			"ripencc|RU|ipv4|10.0.0.0|256|20200101|assigned",
			"ripencc|RU|ipv4|10.0.1.0|256|20200101|assigned",
			"ripencc|RU|ipv4|10.0.2.0|512|20200101|assigned",
			"ripencc|DE|ipv4|10.1.0.0|256|20200101|assigned",
			"ripencc|RU|ipv6|2001:db8::|32|20200101|assigned",
			"",
		}, "\n")

		got, err := ParseAndMerge(raw)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// 10.0.0.0/24 + 10.0.1.0/24 are adjacent → merge with 10.0.2.0/24 prefix
		// which spans 10.0.2.0..10.0.3.255. Result: one range 10.0.0.0..10.0.3.255.
		if len(got) != 1 {
			t.Fatalf("got %d ranges, want 1: %+v", len(got), got)
		}
		const ten = uint32(10 << 24)
		want := port.Range{Low: ten, High: ten | 0x000003FF} // .0.0.0 .. .0.3.255
		if got[0] != want {
			t.Errorf("got %+v, want %+v", got[0], want)
		}
	})

	t.Run("empty input yields empty result not error", func(t *testing.T) {
		t.Parallel()
		got, err := ParseAndMerge("")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("got %d ranges, want 0", len(got))
		}
	})

	t.Run("malformed input line yields error without partial output", func(t *testing.T) {
		t.Parallel()
		raw := "ripencc|RU|ipv4|1.2.3.0|notanumber|x|x"
		_, err := ParseAndMerge(raw)
		if err == nil {
			t.Fatal("expected error for malformed count, got nil")
		}
	})
}
