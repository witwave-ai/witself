package server

import (
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestGoMemoryMetrics(t *testing.T) {
	response := httptest.NewRecorder()
	metricsMux().ServeHTTP(response, httptest.NewRequest("GET", "/metrics", nil))
	body := response.Body.String()
	for _, name := range []string{
		"witself_go_heap_live_bytes", "witself_go_heap_goal_bytes",
		"witself_go_memory_total_bytes", "witself_go_memory_limit_bytes",
		"witself_go_gc_cycles_total", "witself_container_memory_limit_bytes",
	} {
		kind := "gauge"
		if name == "witself_go_gc_cycles_total" {
			kind = "counter"
		}
		if !strings.Contains(body, "# HELP "+name+" ") || !strings.Contains(body, "# TYPE "+name+" "+kind+"\n") {
			t.Errorf("missing HELP/TYPE for %s", name)
		}
		matches := regexp.MustCompile(`(?m)^`+name+` ([0-9]+)$`).FindAllStringSubmatch(body, -1)
		if len(matches) != 1 {
			t.Fatalf("want exactly one integer value for %s", name)
		}
		value, err := strconv.ParseInt(matches[0][1], 10, 64)
		if err != nil {
			t.Fatalf("invalid integer for %s", name)
		}
		if name == "witself_go_memory_total_bytes" && value <= 0 {
			t.Error("Go total must be positive")
		}
	}
}
