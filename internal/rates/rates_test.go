package rates_test

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/justskiv/rates/internal/rates"
)

func sample() rates.Snapshot {
	return rates.Snapshot{
		SchemaVersion: 1,
		Base:          "USD",
		AsOf:          rates.NewTime(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)),
		GeneratedAt:   rates.NewTime(time.Date(2026, 11, 1, 19, 7, 12, 999, time.UTC)),
		Source:        "frankfurter-v2-blend",
		Rates:         map[rates.Code]float64{"USD": 1, "EUR": 0.89478, "JPY": 157.66, "IRR": 1865170, "KWD": 0.30963, "BBD": 2},
		Flags:         map[rates.Code][]rates.Flag{"BBD": {rates.FlagPegged}, "IRR": {rates.FlagDisputed, rates.FlagMultiRate}},
	}
}

func TestEncodeGolden(t *testing.T) {
	got, err := rates.Encode(sample())
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schemaVersion":1,"base":"USD","asOf":"2026-11-01T00:00:00Z",` +
		`"generatedAt":"2026-11-01T19:07:12Z","source":"frankfurter-v2-blend",` +
		`"rates":{"BBD":2,"EUR":0.89478,"IRR":1865170,"JPY":157.66,"KWD":0.30963,"USD":1},` +
		`"flags":{"BBD":["pegged"],"IRR":["disputed","multiRate"]}}`
	if string(got) != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestEncodeRoundTrip(t *testing.T) {
	b, err := rates.Encode(sample())
	if err != nil {
		t.Fatal(err)
	}
	s, err := rates.Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s, sample()) {
		t.Errorf("round trip changed the snapshot:\n%+v\n%+v", s, sample())
	}
	again, err := rates.Encode(s)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(b) {
		t.Error("re-encoding changed the bytes")
	}
}

func TestEncodeOmitsZeroOptionals(t *testing.T) {
	s := sample()
	s.GeneratedAt, s.Source, s.Flags = rates.Time{}, "", nil
	b, err := rates.Encode(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"generatedAt", "source", "flags", "alt", "endpoints", "revised"} {
		if strings.Contains(string(b), `"`+field+`"`) {
			t.Errorf("%s present in %s", field, b)
		}
	}
}

func TestEncodeRejectsUnsortedFlags(t *testing.T) {
	s := sample()
	s.Flags["IRR"] = []rates.Flag{rates.FlagMultiRate, rates.FlagDisputed}
	if _, err := rates.Encode(s); err == nil {
		t.Error("unsorted flags encoded")
	}
}

func TestEncodeNoExponentInBounds(t *testing.T) {
	for _, v := range []float64{rates.MinRate * 1.0001, 0.000123456, 1, 99999999.9} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if strings.ContainsAny(string(b), "eE") {
			t.Errorf("%v encodes as %s", v, b)
		}
	}
}

func TestTimeJSON(t *testing.T) {
	tests := []struct {
		give    string
		wantErr bool
	}{
		{give: `"2026-11-01T19:07:12Z"`},
		{give: `"2026-11-01T19:07:12.5Z"`, wantErr: true},
		{give: `"2026-11-01T19:07:12+05:00"`, wantErr: true},
		{give: `"2026-11-01"`, wantErr: true},
		{give: `1793491351`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.give, func(t *testing.T) {
			var tm rates.Time
			err := json.Unmarshal([]byte(tt.give), &tm)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			b, err := json.Marshal(tm)
			if err != nil || string(b) != tt.give {
				t.Errorf("re-encoded as %s (%v)", b, err)
			}
		})
	}
}

func TestNewTimeTruncates(t *testing.T) {
	got := rates.NewTime(time.Date(2026, 11, 1, 19, 7, 12, 999_999_999, time.FixedZone("x", 5*3600)))
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if want := `"2026-11-01T14:07:12Z"`; string(b) != want {
		t.Errorf("got %s, want %s", b, want)
	}
}

func TestRound(t *testing.T) {
	tests := []struct {
		give   float64
		digits int
		want   float64
	}{
		{0.892532810, 6, 0.892533},
		{1865174, 6, 1865170},
		{13003.87495333, 6, 13003.9},
		{586.9369, 7, 586.9369},
		{2, 6, 2},
	}
	for _, tt := range tests {
		if got := rates.Round(tt.give, tt.digits); got != tt.want {
			t.Errorf("Round(%v, %d) = %v, want %v", tt.give, tt.digits, got, tt.want)
		}
	}
}

func TestValidCodeAndRate(t *testing.T) {
	for _, c := range []string{"USD", "XCG"} {
		if !rates.ValidCode(c) {
			t.Errorf("%s rejected", c)
		}
	}
	for _, c := range []string{"usd", "US", "USDT", "U$D", ""} {
		if rates.ValidCode(c) {
			t.Errorf("%q accepted", c)
		}
	}
	for _, v := range []float64{0, -1, 1e-4, 1e8, math.Inf(1), math.NaN()} {
		if rates.ValidRate(v) {
			t.Errorf("rate %v accepted", v)
		}
	}
}

func TestPrimaryCount(t *testing.T) {
	s := sample()
	s.Flags["JPY"] = []rates.Flag{rates.FlagSecondary}
	s.Flags["KWD"] = []rates.Flag{rates.FlagStale}
	// USD, EUR, IRR (disputed), BBD (pegged) are first-hand.
	if got := s.PrimaryCount(); got != 4 {
		t.Errorf("PrimaryCount = %d, want 4", got)
	}
}
