package registry

import "testing"

// resolve 拒绝路径逃逸与绝对路径。
func TestResolveRejects(t *testing.T) {
	s := &server{root: t.TempDir()}
	for _, rel := range []string{"..", "../x", "/etc/passwd", "../../x"} {
		if _, err := s.resolve(rel); err == nil {
			t.Fatalf("%q 应拒绝", rel)
		}
	}
	if _, err := s.resolve("artifacts/p.zip"); err != nil {
		t.Fatalf("合法相对路径应通过: %v", err)
	}
}
