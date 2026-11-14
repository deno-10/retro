package store

import (
	"errors"
	"path/filepath"
	"testing"

	"retro/internal/model"
)

func TestGetForMemberOnLegacyAndPrivateBoards(t *testing.T) {
	for _, backend := range []string{"json", "db", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			var st Store
			if backend == "postgres" {
				st, _ = postgresPair(t)
			} else {
				local, err := Open(filepath.Join(t.TempDir(), "boards."+backend))
				if err != nil {
					t.Fatal(err)
				}
				defer local.Close()
				st = local
			}
			const owner, author = "owner-000001", "author-00001"
			board, err := model.NewBoard("Старое ретро", "classic", owner)
			if err != nil {
				t.Fatal(err)
			}
			// Запись прежнего формата без поля members, с автором и участником этапа.
			if _, err = board.JoinWriting(author, 1); err != nil {
				t.Fatal(err)
			}
			if _, err = board.AddCard(board.Columns[0].ID, "Карточка автора", "", author); err != nil {
				t.Fatal(err)
			}
			if err = st.Create(board); err != nil {
				t.Fatal(err)
			}
			for _, actor := range []string{author, "", "uninvited-123", "x' OR 1=1 --", "$.ownerId"} {
				got, err := st.GetForMember(board.ID, actor)
				if got != nil || !errors.Is(err, model.ErrNotFound) {
					t.Fatalf("получена чужая доска для %q: %v", actor, err)
				}
			}
			got, err := st.GetForMember(board.ID, owner)
			if err != nil || got.Columns[0].Cards[0].Text != "Карточка автора" {
				t.Fatalf("ведущий потерял старую доску: %v", err)
			}
			if _, err = st.Update(board.ID, func(b *model.Board) error {
				_, err := b.AddMember(author, owner)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if got, err := st.GetForMember(board.ID, author); err != nil || got.ID != board.ID {
				t.Fatalf("после приглашения доска недоступна: %v", err)
			}
		})
	}
}
