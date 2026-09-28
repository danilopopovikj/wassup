package redisprobe

import "testing"

// FuzzParseInfo feeds ParseInfo what a server might answer to INFO. The text
// comes off the network, so no answer may crash the probe, and a number is
// only reported for a field the server sent.
func FuzzParseInfo(f *testing.F) {
	for _, s := range []string{
		infoFixture,
		"db0:keys=,expires",
		"db0:keys=1e400,expires=-1,avg_ttl=NaN",
		"used_memory: 12 \r\nmaxmemory:\r\n",
		":",
		"",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, text string) {
		info := ParseInfo(text)
		for _, db := range info.Keyspace {
			if _, ok := info.Fields[db.Name]; !ok {
				t.Fatalf("keyspace line %q is missing from the fields", db.Name)
			}
		}
		for key := range info.Fields {
			info.Num(key)
		}
		if _, ok := info.Num("\x00 never sent"); ok {
			t.Fatal("a number was reported for a field the server did not send")
		}
		info.Keys()
	})
}
