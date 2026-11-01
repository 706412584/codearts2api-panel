package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPanelShowsBuildVersion(t *testing.T) {
	original := Version
	t.Cleanup(func() { Version = original })
	h := NewHandler(Config{APIKey: "test-key"})
	for _, test := range []struct{ version, rendered string }{
		{"v26.11.01.2", "v26.11.01.2"},
		{`v26.11.01.3<script>alert("build")</script>`, `v26.11.01.3&lt;script&gt;alert(&#34;build&#34;)&lt;/script&gt;`},
	} {
		t.Run(test.version, func(t *testing.T) {
			Version = test.version
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/panel", nil))
			if w.Code != http.StatusOK {
				t.Fatalf("panel returned %d", w.Code)
			}
			body := w.Body.String()
			if !strings.Contains(body, "<small>CodeArts · "+test.rendered+"</small>") {
				t.Fatalf("panel did not display escaped build version %q", test.rendered)
			}
			if strings.Contains(body, "{{VERSION}}") || strings.Contains(body, "cn-north-4") || strings.Contains(body, "<script>") {
				t.Fatal("panel retained a placeholder, region, or unescaped version markup")
			}
		})
	}
}
