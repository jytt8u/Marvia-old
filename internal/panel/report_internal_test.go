package panel

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Бот получает готовое сообщение продавца с подставленным именем и датой —
// и не получает поля text, если продавец текста к событию не писал.
func TestBotGetsTheSellersTextFilledIn(t *testing.T) {
	bot := &webhookBot{}
	s, d := openWebhookStore(t, filepath.Join(t.TempDir(), "panel.db"), bot)
	ctx := context.Background()
	if err := s.SetBotTexts(ctx, BotTexts{EventUserCreate: "Привет, {name}! Доступ до {date}, осталось {left}."}); err != nil {
		t.Fatal(err)
	}
	until := &Expiry{time.Date(2027, 1, 31, 12, 0, 0, 0, moscow)}
	if _, _, err := s.CreateUser(ctx, CreateUserParams{Label: "Анна", ExpiresAt: until, TrafficLimit: 50 << 30}); err != nil {
		t.Fatal(err)
	}
	if err := d.Deliver(ctx, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	var got WebhookEvent
	if err := json.Unmarshal(bot.bodies[0], &got); err != nil {
		t.Fatal(err)
	}
	if got.Text != "Привет, Анна! Доступ до 31.01.2027, осталось 50.0 ГБ." {
		t.Fatalf("текст: %q", got.Text)
	}

	if _, _, err := s.CreateUser(ctx, CreateUserParams{Label: "Борис"}); err != nil {
		t.Fatal(err)
	}
	_ = s.SetBotTexts(ctx, BotTexts{})
	if _, _, err := s.CreateUser(ctx, CreateUserParams{Label: "Вера"}); err != nil {
		t.Fatal(err)
	}
	if err := d.Deliver(ctx, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if last := string(bot.bodies[len(bot.bodies)-1]); strings.Contains(last, `"text"`) {
		t.Fatalf("без текста продавца поле text всё равно пришло: %s", last)
	}
	if err := s.SetBotTexts(ctx, BotTexts{"user.typo": "x"}); err == nil {
		t.Fatal("текст к несуществующему событию принят")
	}
}

// Отчёт считает месяц по Москве: новые, продления, промокоды и тех, кто
// ушёл, не продлив.
func TestMonthReportCountsTheMonth(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	month := time.Now().In(moscow).Format("2006-01")
	past := &Expiry{time.Now().Add(-time.Minute)}
	if _, _, err := s.CreateUser(ctx, CreateUserParams{Label: "ушёл", ExpiresAt: past}); err != nil {
		t.Fatal(err)
	}
	u, _, err := s.CreateUser(ctx, CreateUserParams{Label: "остался"})
	if err != nil {
		t.Fatal(err)
	}
	s.Record(ctx, Event{Action: EventUserRenew, UserID: u.ID})
	rep, err := s.MonthReport(ctx, month, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if rep.New != 2 || rep.Renewals != 1 {
		t.Fatalf("новых %d, продлений %d", rep.New, rep.Renewals)
	}
	// «Ушёл» может оказаться в прошлом месяце, если тест бежит в первую
	// минуту месяца; тогда его в этом месяце и нет.
	if first, _, _ := monthBounds(month); time.Now().Add(-time.Minute).After(first) && rep.Lapsed != 1 {
		t.Fatalf("не продливших %d, ждали 1", rep.Lapsed)
	}
	if _, err := s.MonthReport(ctx, "октябрь", time.Now()); err == nil {
		t.Fatal("кривой месяц принят")
	}
}
