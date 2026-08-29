package store

import "testing"

func TestDebateReviewRoundtrip(t *testing.T) {
	db, err := NewDB(":memory:")
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	defer db.Close()
	repo := NewDebateReviewRepo(db)

	id, err := repo.Insert(&DebateReview{
		DebateID: 42, TradeDate: "20260820", TsCode: "600519.SH",
		Decision: "buy", Confidence: 0.7, BaseClose: 100.0,
		ReviewDate: "20260827", LastClose: 105.0, RetPct: 5.0, Correct: 1,
	})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if id <= 0 {
		t.Fatalf("id=%d", id)
	}

	got, err := repo.GetRecentByCode("600519.SH", 5)
	if err != nil {
		t.Fatalf("GetRecentByCode: %v", err)
	}
	if len(got) != 1 || got[0].DebateID != 42 || got[0].RetPct != 5.0 || got[0].Correct != 1 {
		t.Fatalf("roundtrip 不匹配: %+v", got)
	}
}
