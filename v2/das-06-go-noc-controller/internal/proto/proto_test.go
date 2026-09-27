package proto

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// Produced by Node.js 22: JSON.stringify(v) as input, then the reference canonical()
// of agent/noc-agent.js and its FNV-1a 64 (see docs/PROTOCOL.md).
const (
	nodeInput     = "{\"name\":\"Terminal A & B <North> \\\"Gate\\\" \\\\ 7\",\"venue\":\"Aéroport Nord \U0001f600\",\"ctl\":\"\\u0001\\u001f\\b\\f\\n\\r\\t\u007f\",\"nums\":[0,0,1e+21,1e-7,0.000001,0.30000000000000004,123456789012,-1.5e-10,5e-324,1.7976931348623157e+308,48.25,-97.12,100],\"alarms\":{\"minor\":2,\"critical\":0,\"major\":1,\"warning\":0},\"fwMix\":{\"4.2.1\":270,\"4.1.7\":30},\"ok\":true,\"none\":null,\"nested\":[{\"b\":1,\"a\":[true,false]}]}"
	nodeCanonical = "{\"alarms\":{\"critical\":0,\"major\":1,\"minor\":2,\"warning\":0},\"ctl\":\"\\u0001\\u001f\\b\\f\\n\\r\\t\u007f\",\"fwMix\":{\"4.1.7\":30,\"4.2.1\":270},\"name\":\"Terminal A & B <North> \\\"Gate\\\" \\\\ 7\",\"nested\":[{\"a\":[true,false],\"b\":1}],\"none\":null,\"nums\":[0,0,1e+21,1e-7,0.000001,0.30000000000000004,123456789012,-1.5e-10,5e-324,1.7976931348623157e+308,48.25,-97.12,100],\"ok\":true,\"venue\":\"Aéroport Nord \U0001f600\"}"
	nodeHash      = "8a26ed578378ebf3"
)

func TestCanonicalMatchesNodeJS(t *testing.T) {
	var v any
	if err := json.Unmarshal([]byte(nodeInput), &v); err != nil {
		t.Fatal(err)
	}
	got, err := AppendCanonical(nil, v)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != nodeCanonical {
		t.Fatalf("canonical differs from Node.js:\n got %q\nwant %q", got, nodeCanonical)
	}
	if h := HashHex(FNV1a64(got)); h != nodeHash {
		t.Fatalf("hash %s, Node.js %s", h, nodeHash)
	}
	var raw map[string]json.RawMessage
	_ = json.Unmarshal([]byte(nodeInput), &raw)
	if h, _ := SummaryHash(raw); h != nodeHash {
		t.Fatalf("SummaryHash %s", h)
	}
	if HashHex(FNV1a64(nil)) != "cbf29ce484222325" || HashHex(FNV1a64([]byte("a"))) != "af63dc4c8601ec8c" {
		t.Fatal("FNV-1a 64 test vectors")
	}
}

func TestJSNumberEdgeCases(t *testing.T) {
	for _, c := range []struct {
		f    float64
		want string
	}{
		{math.Copysign(0, -1), "0"}, {math.NaN(), "null"}, {math.Inf(1), "null"},
		{1e21, "1e+21"}, {999999999999999900000, "999999999999999900000"}, {1e-7, "1e-7"}, {1.5e-7, "1.5e-7"},
		{1e-6, "0.000001"}, {-1e-100, "-1e-100"}, {2.5, "2.5"}, {-97.12, "-97.12"},
	} {
		if got := string(AppendJSNumber(nil, c.f)); got != c.want {
			t.Errorf("%v: got %s, want %s", c.f, got, c.want)
		}
	}
}

func TestTokens(t *testing.T) {
	secret := []byte("s3cret")
	site, err := Mint(secret, Claims{Kind: KindSite, Subject: "S00042", Tenant: "airport-north"})
	if err != nil {
		t.Fatal(err)
	}
	admin, _ := Mint(secret, Claims{Kind: KindUser, Subject: "noc-alice", Tenant: AllTenants})
	v := &Verifier{Secrets: [][]byte{[]byte("new"), secret}} // rotation: old tokens still valid
	c, err := v.Verify(site)
	if err != nil || c.Subject != "S00042" || c.Tenant != "airport-north" || c.Kind != KindSite || c.Admin() {
		t.Fatalf("site token: %+v %v", c, err)
	}
	if c, err := v.Verify(admin); err != nil || !c.Admin() || !c.Sees("anything") {
		t.Fatalf("admin token: %+v %v", c, err)
	}
	if _, err := Mint(secret, Claims{Kind: KindSite, Subject: "S1", Tenant: AllTenants}); err == nil {
		t.Fatal("a device cannot hold an all-tenants token")
	}
	tampered := strings.Replace(site, "S00042", "S00043", 1)
	if _, err := v.Verify(tampered); err != ErrBadSig {
		t.Fatalf("tampered token: %v", err)
	}
	if _, err := (&Verifier{Secrets: [][]byte{[]byte("other")}}).Verify(site); err != ErrBadSig {
		t.Fatal("wrong secret accepted")
	}
	v.SetRevoked(map[string]bool{"site:S00042": true})
	if _, err := v.Verify(site); err == nil {
		t.Fatal("revoked token accepted")
	}
	if TokenFrom("", []string{Subprotocol, "bearer." + site}) != site || TokenFrom("Bearer "+site, nil) != site {
		t.Fatal("TokenFrom")
	}
}

// The worked examples in docs/PROTOCOL.md (hashes computed with agent/noc-agent.js).
func TestProtocolDocExamples(t *testing.T) {
	for in, want := range map[string]string{
		`{"name":"Terminal 2","venue":"Airport North","region":"north","fw":"3.1.4","nodes":300,"online":298,"offline":2,"degraded":1,"maxTempC":47.5,"minRxDbm":-8.3,"maxVswr":1.31}`: "0cc469c8e64bfdee",
		`{"name":"Terminal 2","venue":"Airport North","region":"north","fw":"3.1.4","nodes":300,"online":299,"offline":1,"degraded":1,"maxTempC":48,"minRxDbm":-8.3,"maxVswr":1.31}`:   "4ff86457e6641c6a",
		`{"b":[1,2,{"d":null,"c":"é"}],"a":1e21,"z":0.1}`: "9e3cfb1f27fec5aa",
		`{}`: "08f44b07b5901a25",
	} {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(in), &raw); err != nil {
			t.Fatal(err)
		}
		if h, _ := SummaryHash(raw); h != want {
			t.Errorf("%s: %s, want %s", in, h, want)
		}
	}
}
