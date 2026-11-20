package httpapi_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func listItems(t *testing.T, r response) []map[string]any {
	t.Helper()
	if r.Status != 200 {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
	var out []map[string]any
	for _, it := range r.json()["items"].([]any) {
		out = append(out, it.(map[string]any))
	}
	return out
}

func listIDs(t *testing.T, r response) string {
	t.Helper()
	var ids []string
	for _, it := range listItems(t, r) {
		ids = append(ids, it["id"].(string))
	}
	return strings.Join(ids, ",")
}

func TestDeliveryListFailureReasonFilter(t *testing.T) {
	s := newServer(t, false)
	fs := newFailSeed(t, s, "Target")
	now := s.env.Clock.Now()
	org := "/api/v1/organizations/" + fs.is.org

	byReason := map[string][]string{}
	n := 0
	seed := func(reason string, count int) {
		for i := 0; i < count; i++ {
			n++
			byReason[reason] = append(byReason[reason], fs.failed(now.Add(-time.Duration(n)*time.Minute), reason))
		}
	}
	seed("RETRIES_EXHAUSTED", 5)
	seed("DESTINATION_REJECTED", 2)
	seed("REDIRECT_REFUSED", 1)
	seed("EGRESS_BLOCKED", 1)
	seed("INTERNAL_ERROR", 1)
	seed("", 3) // legacy rows
	// A delivery that has not failed.
	ev, err := s.env.Send(fs.is.pub, s.env.Signed(fs.is.secret, []byte(`{"p":1}`), "pending-filter"))
	if err != nil {
		t.Fatal(err)
	}
	pending := ""
	if err := mustQuery(s, "INSERT INTO deliveries (organization_id, event_id, destination_id) VALUES ('"+fs.is.org+"', '"+ev.EventID+"', '"+fs.dest+"') RETURNING id::text", &pending); err != nil {
		t.Fatal(err)
	}

	t.Run("each reason selects exactly its deliveries and the total is exact", func(t *testing.T) {
		for reason, want := range byReason {
			filter := reason
			if reason == "" {
				filter = "UNRECORDED"
			}
			r := fs.is.c.do("GET", org+"/deliveries?failureReason="+filter+"&limit=100", nil)
			if r.Status != 200 || r.json()["total"] != float64(len(want)) {
				t.Errorf("%s: %d %s", filter, r.Status, r.Body)
				continue
			}
			got := map[string]bool{}
			for _, it := range listItems(t, r) {
				got[it["id"].(string)] = true
				if it["status"] != "FAILED" {
					t.Errorf("%s: %v", filter, it)
				}
				switch reason {
				case "":
					// The existing list omits a NULL reason, and keeps doing so under the filter.
					if _, has := it["failureReason"]; has {
						t.Errorf("a legacy row must keep omitting failureReason: %v", it)
					}
				default:
					if it["failureReason"] != reason {
						t.Errorf("%s: %v", filter, it)
					}
				}
			}
			for _, id := range want {
				if !got[id] {
					t.Errorf("%s: missing %s", filter, id)
				}
			}
			if len(got) != len(want) {
				t.Errorf("%s: %d items, want %d", filter, len(got), len(want))
			}
		}
	})

	t.Run("offset pagination keeps working and the total stays exact on every page", func(t *testing.T) {
		var seen []string
		for offset := 0; offset < 6; offset += 2 {
			r := fs.is.c.do("GET", org+"/deliveries?failureReason=RETRIES_EXHAUSTED&limit=2&offset="+string(rune('0'+offset)), nil)
			if r.Status != 200 || r.json()["total"] != float64(5) || r.json()["limit"] != float64(2) || r.json()["offset"] != float64(offset) {
				t.Fatalf("offset %d: %d %s", offset, r.Status, r.Body)
			}
			for _, it := range listItems(t, r) {
				seen = append(seen, it["id"].(string))
			}
		}
		if len(seen) != 5 {
			t.Errorf("paged %d, want 5: %v", len(seen), seen)
		}
		// Ordering is the list's own: newest creation first.
		r := fs.is.c.do("GET", org+"/deliveries?failureReason=RETRIES_EXHAUSTED&limit=100", nil)
		prev := ""
		for _, it := range listItems(t, r) {
			created := it["createdAt"].(string)
			if prev != "" && created > prev {
				t.Errorf("the filtered list keeps created_at DESC: %s after %s", created, prev)
			}
			prev = created
		}
	})

	t.Run("the status a reason implies is accepted and any other status is a 400", func(t *testing.T) {
		if r := fs.is.c.do("GET", org+"/deliveries?failureReason=RETRIES_EXHAUSTED&status=FAILED", nil); r.Status != 200 || r.json()["total"] != float64(5) {
			t.Errorf("status=FAILED: %d %s", r.Status, r.Body)
		}
		for _, st := range []string{"PENDING", "IN_FLIGHT", "SUCCEEDED", "CANCELLED", "BOGUS"} {
			for _, reason := range []string{"RETRIES_EXHAUSTED", "UNRECORDED"} {
				r := fs.is.c.do("GET", org+"/deliveries?failureReason="+reason+"&status="+st, nil)
				if r.Status != 400 || r.errorCode() != "validation_failed" {
					t.Errorf("failureReason=%s&status=%s: %d %s, want 400", reason, st, r.Status, r.Body)
				}
			}
		}
	})

	t.Run("unknown values are a 400, an empty value is no filter", func(t *testing.T) {
		for _, bad := range []string{"nope", "retries_exhausted", "unrecorded", "FAILED", "RETRIES_EXHAUSTED%27%20OR%20%271%27%3D%271", "%20"} {
			r := fs.is.c.do("GET", org+"/deliveries?failureReason="+bad, nil)
			if r.Status != 400 || r.errorCode() != "validation_failed" || strings.Contains(r.Body, "SQLSTATE") {
				t.Errorf("failureReason=%s: %d %s", bad, r.Status, r.Body)
			}
		}
		if r := fs.is.c.do("GET", org+"/deliveries?failureReason=", nil); r.Status != 200 || r.json()["total"] != float64(14) {
			t.Errorf("an empty failureReason is no filter: %d %s", r.Status, r.Body)
		}
	})

	t.Run("it combines with the other filters and stays inside the organization", func(t *testing.T) {
		r := fs.is.c.do("GET", org+"/deliveries?failureReason=DESTINATION_REJECTED&destinationId="+fs.dest, nil)
		if r.Status != 200 || r.json()["total"] != float64(2) {
			t.Errorf("with the destination: %d %s", r.Status, r.Body)
		}
		r = fs.is.c.do("GET", org+"/deliveries?failureReason=DESTINATION_REJECTED&destinationId=00000000-0000-4000-8000-000000000000", nil)
		if r.Status != 200 || r.json()["total"] != float64(0) {
			t.Errorf("with a foreign destination: %d %s", r.Status, r.Body)
		}
		r = fs.is.c.do("GET", org+"/deliveries?failureReason=RETRIES_EXHAUSTED&from="+now.Add(-3*time.Hour).Format(time.RFC3339)+"&to="+now.Add(time.Hour).Format(time.RFC3339), nil)
		if r.Status != 200 || r.json()["total"] != float64(5) {
			t.Errorf("with a window: %d %s", r.Status, r.Body)
		}
		other := newFailSeedAs(t, s, "filter-other@example.com", "Other")
		if r := other.is.c.do("GET", "/api/v1/organizations/"+other.is.org+"/deliveries?failureReason=RETRIES_EXHAUSTED", nil); r.Status != 200 || r.json()["total"] != float64(0) {
			t.Errorf("another organization sees none of it: %d %s", r.Status, r.Body)
		}
		if r := other.is.c.do("GET", org+"/deliveries?failureReason=RETRIES_EXHAUSTED", nil); r.Status != 404 || strings.Contains(r.Body, "RETRIES_EXHAUSTED") {
			t.Errorf("a foreign organization path: %d %s", r.Status, r.Body)
		}
	})

	t.Run("authorization comes before validation", func(t *testing.T) {
		stranger := s.client(t)
		stranger.register("filter-stranger@example.com")
		for _, q := range []string{"failureReason=nope", "failureReason=RETRIES_EXHAUSTED&status=PENDING"} {
			if r := stranger.do("GET", org+"/deliveries?"+q, nil); r.Status != 404 || r.errorCode() != "not_found" {
				t.Errorf("non-member ?%s: %d %s", q, r.Status, r.Body)
			}
		}
		if r := s.client(t).do("GET", org+"/deliveries?failureReason=nope", nil); r.Status != 401 {
			t.Errorf("anonymous: %d %s", r.Status, r.Body)
		}
		for _, role := range []string{"ADMIN", "DEVELOPER", "VIEWER"} {
			c := join(t, fs.is.c, s, fs.is.org, "filter-"+strings.ToLower(role)+"@example.com", role)
			if r := c.do("GET", org+"/deliveries?failureReason=RETRIES_EXHAUSTED", nil); r.Status != 200 || r.json()["total"] != float64(5) {
				t.Errorf("%s: %d %s", role, r.Status, r.Body)
			}
		}
	})

	t.Run("a request without the parameter is unchanged, byte for byte", func(t *testing.T) {
		// An empty failureReason is no filter, so the two requests must give identical bytes.
		plain := fs.is.c.do("GET", org+"/deliveries?status=FAILED&limit=100", nil)
		empty := fs.is.c.do("GET", org+"/deliveries?status=FAILED&limit=100&failureReason=", nil)
		if plain.Status != 200 || plain.Body != empty.Body {
			t.Errorf("an empty failureReason changed the response:\n%s\n%s", plain.Body, empty.Body)
		}
		// The response shape of the existing endpoint: top-level fields, item fields, and the omission rules.
		var top map[string]json.RawMessage
		_ = json.Unmarshal([]byte(plain.Body), &top)
		if keysOf(map[string]any{"items": 0, "total": 0, "limit": 0, "offset": 0}) != keysRaw(top) {
			t.Errorf("top-level fields: %s", keysRaw(top))
		}
		for _, it := range listItems(t, plain) {
			want := "attemptCount,completedAt,createdAt,destinationId,destinationName,eventId,id,lastErrorClass,lastHttpStatus,maxAttempts,organizationId,status,updatedAt"
			if _, has := it["failureReason"]; has {
				want = "attemptCount,completedAt,createdAt,destinationId,destinationName,eventId,failureReason,id,lastErrorClass,lastHttpStatus,maxAttempts,organizationId,status,updatedAt"
			}
			if keysOf(it) != want {
				t.Errorf("item fields: %s\nwant %s", keysOf(it), want)
			}
		}
		// An item is the same bytes with or without the filter.
		all := rawItems(t, plain)
		filtered := rawItems(t, fs.is.c.do("GET", org+"/deliveries?failureReason=RETRIES_EXHAUSTED&limit=100", nil))
		for id, raw := range filtered {
			if all[id] != raw {
				t.Errorf("item %s differs under the filter:\n%s\n%s", id, all[id], raw)
			}
		}
		if len(filtered) != 5 {
			t.Errorf("%d filtered items", len(filtered))
		}
		// The unrelated pending delivery is still listed without a filter.
		if r := fs.is.c.do("GET", org+"/deliveries", nil); !strings.Contains(listIDs(t, r), pending) {
			t.Errorf("the unfiltered list still lists every status")
		}
	})
}

func keysRaw(m map[string]json.RawMessage) string {
	out := map[string]any{}
	for k := range m {
		out[k] = 0
	}
	return keysOf(out)
}

// rawItems maps a list response's item ids to their exact JSON text.
func rawItems(t *testing.T, r response) map[string]string {
	t.Helper()
	var top struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal([]byte(r.Body), &top); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, raw := range top.Items {
		var it struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(raw, &it)
		out[it.ID] = string(raw)
	}
	return out
}
