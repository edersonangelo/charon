package web_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/edersonangelo/charon/internal/postgres"
)

// The smallest page a reader can ask for, and enough events to need three of
// them. Sizes outside the closed set are not pageable at all, so the fixtures
// are built from the ones that are.
const (
	smallest = 25
	plenty   = 60
)

func recordMany(t *testing.T, store *postgres.Store, n int) {
	t.Helper()

	for i := range n {
		recordAt(t, store, "pager", arrived.Add(time.Duration(i)*time.Minute))
	}
}

// A page that came back full does not mean there is another one behind it.
func TestTheLastPageOffersNoNextEvenWhenItIsFull(t *testing.T) {
	t.Parallel()

	store, server, client, _ := setup(t)
	recordMany(t, store, smallest)
	signIn(t, server, client, password)

	_, page := get(t, client, server.URL+"/events?provider=pager&size=25")

	if !strings.Contains(page, "showing 1–25") {
		t.Errorf("the footer does not say what is on screen:\n%s", footer(page))
	}
	if strings.Contains(page, "next ›") {
		t.Errorf("a full last page still offered a next page:\n%s", footer(page))
	}
	if strings.Contains(page, "‹ previous") {
		t.Errorf("the first page offered a previous page:\n%s", footer(page))
	}
}

func TestPagingForwardAndBackKeepsCountingFromTheSamePlace(t *testing.T) {
	t.Parallel()

	store, server, client, _ := setup(t)
	recordMany(t, store, plenty)
	signIn(t, server, client, password)

	first := server.URL + "/events?provider=pager&size=25"
	_, one := get(t, client, first)
	if !strings.Contains(one, "showing 1–25") || !strings.Contains(one, "next ›") {
		t.Errorf("the first of three pages is wrong:\n%s", footer(one))
	}

	_, three := get(t, client, first+"&page=3")
	if !strings.Contains(three, "showing 51–60") {
		t.Errorf("the last page does not say it holds the remainder:\n%s", footer(three))
	}
	if strings.Contains(three, "next ›") {
		t.Errorf("the last page offered a next page:\n%s", footer(three))
	}
	if !strings.Contains(three, "‹ previous") {
		t.Errorf("the last page offered no way back:\n%s", footer(three))
	}
}

// The size reaches a limit clause, so anything outside the closed set is the
// default rather than whatever was typed.
func TestASizeNobodyOffersFallsBackToTheDefault(t *testing.T) {
	t.Parallel()

	store, server, client, _ := setup(t)
	recordMany(t, store, smallest)
	signIn(t, server, client, password)

	for _, asked := range []string{"999", "0", "-5", "banana"} {
		_, page := get(t, client, server.URL+"/events?provider=pager&size="+asked)

		if !strings.Contains(page, `<option value="50" selected>`) {
			t.Errorf("size=%s did not fall back to 50:\n%s", asked, footer(page))
		}
		if !strings.Contains(page, "showing 1–25") {
			t.Errorf("size=%s did not hold everything that matched:\n%s", asked, footer(page))
		}
	}
}

func TestEverySizeOfferedIsOneTheListWillHonour(t *testing.T) {
	t.Parallel()

	store, server, client, _ := setup(t)
	recordMany(t, store, smallest)
	signIn(t, server, client, password)

	for _, size := range []int{25, 50, 100, 200} {
		_, page := get(t, client, fmt.Sprintf("%s/events?provider=pager&size=%d", server.URL, size))

		if !strings.Contains(page, fmt.Sprintf(`<option value="%d" selected>`, size)) {
			t.Errorf("size=%d is offered but not remembered:\n%s", size, footer(page))
		}
	}
}

// The control belongs to the filter form above it, so changing how many rows a
// page holds does not throw away what was searched for.
func TestTheSizeControlCarriesTheSearchWithIt(t *testing.T) {
	t.Parallel()

	store, server, client, _ := setup(t)
	recordMany(t, store, plenty)
	signIn(t, server, client, password)

	_, page := get(t, client, server.URL+"/events?provider=pager&size=25")

	if !strings.Contains(page, `<select name="size" form="filters"`) {
		t.Errorf("the size control is not part of the filter form:\n%s", footer(page))
	}
	if !strings.Contains(page, `id="filters"`) {
		t.Errorf("there is no filter form for it to belong to")
	}
	if !strings.Contains(page, "next ›") {
		t.Fatalf("expected more than one page")
	}
	if !strings.Contains(page, "provider=pager") {
		t.Errorf("the link to the next page dropped the search:\n%s", footer(page))
	}
	if !strings.Contains(page, "size=25") {
		t.Errorf("the link to the next page dropped the size:\n%s", footer(page))
	}
}

// footer is the part of the page worth reading when a pagination test fails.
func footer(page string) string {
	at := strings.Index(page, `class="pager"`)
	if at < 0 {
		return "(no pager on the page)"
	}
	return page[at:min(at+600, len(page))]
}
