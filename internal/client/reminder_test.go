package client_test

import (
	"testing"
	"time"

	"github.com/jytt8u/marvia/internal/client"
)

func TestReminderComesThreeDaysAndOneDayBeforeEnd(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	at := func(left time.Duration) client.Subscription {
		return client.Subscription{ExpiresAt: now.Add(left).Format(time.RFC3339)}
	}

	if r := at(4 * 24 * time.Hour).Reminder(now); r.Kind != "" {
		t.Fatalf("за четверо суток уже напоминают: %+v", r)
	}

	three := at(3 * 24 * time.Hour).Reminder(now)
	if three.Kind != client.ReminderExpiry || three.Value != 3 {
		t.Fatalf("за трое суток: %+v", three)
	}
	two := at(2*24*time.Hour + time.Hour).Reminder(now)
	if two.Value != 3 || two.Key != at(2*24*time.Hour+time.Hour).Reminder(now).Key {
		t.Fatalf("на третьи сутки: %+v", two)
	}

	one := at(20 * time.Hour).Reminder(now)
	if one.Kind != client.ReminderExpiry || one.Value != 1 {
		t.Fatalf("за сутки: %+v", one)
	}
	if one.Key == three.Key {
		t.Fatal("напоминания за трое суток и за сутки неотличимы по ключу — второе не покажут")
	}

	if r := at(-time.Hour).Reminder(now); r.Kind != "" {
		t.Fatalf("кончившийся доступ — это отказ, а не напоминание: %+v", r)
	}
}

func TestSameReminderKeepsItsKeyFromDayToDay(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	sub := client.Subscription{ExpiresAt: now.Add(70 * time.Hour).Format(time.RFC3339)}
	if a, b := sub.Reminder(now), sub.Reminder(now.Add(20*time.Hour)); a.Key != b.Key {
		t.Fatalf("одно и то же напоминание назавтра под другим ключом: %q и %q", a.Key, b.Key)
	}
}

func TestReminderGoesAwayAfterRenewal(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	sub := client.Subscription{
		ExpiresAt:    now.Add(20 * time.Hour).Format(time.RFC3339),
		TrafficLimit: 100 << 30, Used: 95 << 30,
	}
	if sub.Reminder(now).Kind == "" {
		t.Fatal("до продления напоминания нет")
	}
	sub.ExpiresAt = now.Add(30 * 24 * time.Hour).Format(time.RFC3339)
	sub.Used = 0
	if r := sub.Reminder(now); r.Kind != "" {
		t.Fatalf("после продления всё ещё напоминают: %+v", r)
	}
}

func TestReminderWhenLessThanTenthOfTrafficLeft(t *testing.T) {
	now := time.Now()
	sub := client.Subscription{TrafficLimit: 1000, Used: 900}
	if r := sub.Reminder(now); r.Kind != "" {
		t.Fatalf("ровно десять процентов — ещё не повод: %+v", r)
	}
	sub.Used = 950
	r := sub.Reminder(now)
	if r.Kind != client.ReminderTraffic || r.Value != 5 {
		t.Fatalf("осталось 5%%: %+v", r)
	}
	sub.Used = 1000
	if r := sub.Reminder(now); r.Kind != "" {
		t.Fatalf("кончившийся трафик — отказ, а не напоминание: %+v", r)
	}
	if r := (client.Subscription{Used: 1 << 40}).Reminder(now); r.Kind != "" {
		t.Fatalf("без лимита напоминают о трафике: %+v", r)
	}
}

func TestLastDayOutranksTrafficAndTrafficOutranksThreeDays(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	sub := client.Subscription{TrafficLimit: 1000, Used: 950}

	sub.ExpiresAt = now.Add(60 * time.Hour).Format(time.RFC3339)
	if r := sub.Reminder(now); r.Kind != client.ReminderTraffic {
		t.Fatalf("за трое суток при 5%% трафика: %+v", r)
	}
	sub.ExpiresAt = now.Add(10 * time.Hour).Format(time.RFC3339)
	if r := sub.Reminder(now); r.Kind != client.ReminderExpiry || r.Value != 1 {
		t.Fatalf("в последний день при 5%% трафика: %+v", r)
	}
}

// «При вашем расходе хватит примерно на 2 дня» — по двум замерам в одном
// периоде; после продления (другой срок) и при всплеске за минуты прогноза нет.
func TestForecastSaysWhenTrafficRunsOut(t *testing.T) {
	const gb = int64(1) << 30
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	cur := client.Subscription{TrafficLimit: 100 * gb, Used: 96 * gb, ExpiresAt: "2026-11-30T00:00:00Z"}
	// За сутки ушло 2 ГБ, осталось 4 — хватит на 2 дня.
	r := client.Forecast(6*gb, 100*gb, cur.ExpiresAt, now.Add(-24*time.Hour), cur, now)
	if r.Kind != client.ReminderForecast || r.Value != 2 {
		t.Fatalf("прогноз: %+v", r)
	}
	if r := client.Forecast(6*gb, 100*gb, "2026-10-30T00:00:00Z", now.Add(-24*time.Hour), cur, now); r.Kind != "" {
		t.Fatalf("прогноз через продление: %+v", r)
	}
	if r := client.Forecast(6*gb, 100*gb, cur.ExpiresAt, now.Add(-10*time.Minute), cur, now); r.Kind != "" {
		t.Fatalf("прогноз по всплеску за минуты: %+v", r)
	}
	// Медленный расход — до конца далеко, напоминать незачем.
	if r := client.Forecast(4*gb+gb/10, 100*gb, cur.ExpiresAt, now.Add(-24*time.Hour), cur, now); r.Kind != "" {
		t.Fatalf("прогноз при медленном расходе: %+v", r)
	}
}
