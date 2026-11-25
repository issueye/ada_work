package compile

import "testing"

func FuzzSourceMap(f *testing.F) {
	f.Add(`{"version":3,"sources":["plugin.ts"],"names":[],"mappings":"AAAA;AAyCA"}`)
	f.Add(`{"version":3,"sources":[],"names":[],"mappings":""}`)
	f.Add(`{}`)
	f.Fuzz(func(t *testing.T, data string) {
		sm, err := ParseSourceMap([]byte(data))
		if err != nil || sm == nil {
			return
		}
		_, _, _, _ = sm.Lookup(1, 0)
		_, _, _, _ = sm.Lookup(100, 20)
	})
}
