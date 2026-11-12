package server

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func addVoteCard(t *testing.T, f *mutationFixture, text string) string {
	t.Helper()
	rec, view := do(t, f.handler, "POST", "/api/boards/"+f.id+"/cards", mutationAuthor, f.payload("cards", text))
	requireMutationStatus(t, rec, http.StatusOK)
	cards := mutationItems(view, "cards")
	return cards[len(cards)-1].(map[string]any)["id"].(string)
}

func groupVoteCards(t *testing.T, f *mutationFixture, first, second string) {
	t.Helper()
	rec, _ := do(t, f.handler, "POST", "/api/boards/"+f.id+"/cards/"+first+"/group", mutationOwner,
		map[string]any{"targetCardId": second})
	requireMutationStatus(t, rec, http.StatusOK)
}

func TestVoteDesiredStateSurvivesLostResponseAndRetry(t *testing.T) {
	for _, backend := range []string{"json", "db"} {
		for _, grouped := range []bool{false, true} {
			name := backend + "/card"
			if grouped {
				name = backend + "/group"
			}
			t.Run(name, func(t *testing.T) {
				f := newMutationFixture(t, backend)
				card := addVoteCard(t, f, "Важное")
				if grouped {
					groupVoteCards(t, f, card, addVoteCard(t, f, "Похожее"))
				}
				f.phase("vote")
				path := "/api/boards/" + f.id + "/cards/" + card + "/vote"
				for _, voted := range []bool{true, false} {
					body := map[string]any{"voted": voted}
					f.handler.ServeHTTP(&lostMutationResponse{headers: http.Header{}},
						mutationRequest(t, "POST", path, mutationAuthor, "", body))
					want := 0
					if voted {
						want = 1
					}
					if got := f.state().VotesUsed(mutationAuthor); got != want {
						t.Fatalf("операция с потерянным ответом: голосов=%d, ожидается %d", got, want)
					}
					rec, view := do(t, f.handler, "POST", path, mutationAuthor, body)
					requireMutationStatus(t, rec, http.StatusOK)
					if got := f.state().VotesUsed(mutationAuthor); got != want {
						t.Fatalf("повтор изменил намерение voted=%t: голосов=%d", voted, got)
					}
					field := "voted"
					if grouped {
						field = "groupVoted"
					}
					if firstCard(t, view)[field] != voted {
						t.Fatalf("представление не отражает voted=%t: %v", voted, view)
					}
				}
			})
		}
	}
}

func TestConcurrentVoteDesiredStateIsStable(t *testing.T) {
	for _, backend := range []string{"json", "db"} {
		t.Run(backend, func(t *testing.T) {
			f := newMutationFixture(t, backend)
			card := addVoteCard(t, f, "Важное")
			f.phase("vote")
			path := "/api/boards/" + f.id + "/cards/" + card + "/vote"
			for _, voted := range []bool{true, false} {
				start := make(chan struct{})
				results := make(chan *httptest.ResponseRecorder, 8)
				var wg sync.WaitGroup
				for range cap(results) {
					req := mutationRequest(t, "POST", path, mutationAuthor, "", map[string]any{"voted": voted})
					wg.Add(1)
					go func() {
						defer wg.Done()
						<-start
						rec := httptest.NewRecorder()
						f.handler.ServeHTTP(rec, req)
						results <- rec
					}()
				}
				close(start)
				wg.Wait()
				close(results)
				for rec := range results {
					requireMutationStatus(t, rec, http.StatusOK)
				}
				want := 0
				if voted {
					want = 1
				}
				if got := f.state().VotesUsed(mutationAuthor); got != want {
					t.Fatalf("одновременные voted=%t оставили %d голосов", voted, got)
				}
			}
		})
	}
}

func TestVoteFalseClearsAllOwnVotesAfterGrouping(t *testing.T) {
	for _, backend := range []string{"json", "db"} {
		t.Run(backend, func(t *testing.T) {
			f := newMutationFixture(t, backend)
			first, second := addVoteCard(t, f, "Первое"), addVoteCard(t, f, "Второе")
			f.phase("vote")
			for _, card := range []string{first, second} {
				rec, _ := do(t, f.handler, "POST", "/api/boards/"+f.id+"/cards/"+card+"/vote", mutationAuthor, map[string]any{"voted": true})
				requireMutationStatus(t, rec, http.StatusOK)
			}
			groupVoteCards(t, f, first, second)
			if f.state().VotesUsed(mutationAuthor) != 2 {
				t.Fatal("до снятия у группы должны быть два прежних голоса")
			}
			for _, card := range []string{first, second} {
				rec, _ := do(t, f.handler, "POST", "/api/boards/"+f.id+"/cards/"+card+"/vote", mutationAuthor, map[string]any{"voted": false})
				requireMutationStatus(t, rec, http.StatusOK)
				if f.state().VotesUsed(mutationAuthor) != 0 {
					t.Fatal("false должен снять все собственные голоса группы")
				}
			}
		})
	}
}

func TestVoteRequiresExplicitBoolean(t *testing.T) {
	for _, backend := range []string{"json", "db"} {
		t.Run(backend, func(t *testing.T) {
			f := newMutationFixture(t, backend)
			card := addVoteCard(t, f, "Важное")
			f.phase("vote")
			path := "/api/boards/" + f.id + "/cards/" + card + "/vote"
			for _, body := range []any{nil, map[string]any{}, map[string]any{"voted": nil}, map[string]any{"voted": "true"}, map[string]any{"voted": 1}} {
				rec, _ := do(t, f.handler, "POST", path, mutationAuthor, body)
				requireMutationStatus(t, rec, http.StatusBadRequest)
			}
			if f.state().VotesUsed(mutationAuthor) != 0 {
				t.Fatal("некорректный запрос изменил голоса")
			}
			rec, _ := do(t, f.handler, "POST", path, mutationAuthor, map[string]any{"voted": false})
			requireMutationStatus(t, rec, http.StatusOK)
		})
	}
}
