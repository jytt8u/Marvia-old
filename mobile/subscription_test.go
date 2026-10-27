package mobile

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jytt8u/marvia/internal/client"
)

// TestScreenGetsSellerLinksAndReminderAsPlainFields: Android читает поля
// подписки плоскими значениями — строки и числа, без вложенных объектов.
func TestScreenGetsSellerLinksAndReminderAsPlainFields(t *testing.T) {
	sub := client.Subscription{
		Nodes:      []client.Node{{ID: 1, Name: "nl", Address: "nl.example:443"}},
		ExpiresAt:  time.Now().Add(20 * time.Hour).UTC().Format(time.RFC3339),
		SupportURL: "tg://resolve?domain=seller_support",
		RenewURL:   "https://t.me/seller_bot",
		Announce:   "Работы ночью",
	}
	raw, err := viewJSON(sub, time.Now(), false)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"support_url":  "tg://resolve?domain=seller_support",
		"renew_url":    "https://t.me/seller_bot",
		"announce":     "Работы ночью",
		"remind":       "expiry",
		"remind_value": float64(1),
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, ожидалось %v", k, got[k], v)
		}
	}
	if key, _ := got["remind_key"].(string); key == "" {
		t.Error("у напоминания нет ключа — приложение будет показывать его при каждом открытии")
	}
}

// TestScreenWithoutSellerSettingsHasNoSellerFields: пустое не отдаётся, и
// приложению не надо отличать "" от «нет поля».
func TestScreenWithoutSellerSettingsHasNoSellerFields(t *testing.T) {
	raw, err := viewJSON(client.Subscription{Nodes: []client.Node{{ID: 1}}}, time.Now(), false)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	_ = json.Unmarshal([]byte(raw), &got)
	for _, k := range []string{"support_url", "renew_url", "announce", "remind", "remind_value", "remind_key"} {
		if _, ok := got[k]; ok {
			t.Errorf("поле %s есть, хотя продавец ничего не задал", k)
		}
	}
}

// TestForecastGivesWayToTheSellersOwnReminder: прогноз — запасной повод.
// Если срок и так на исходе, человек получает одно напоминание о сроке,
// а не два подряд.
func TestForecastGivesWayToTheSellersOwnReminder(t *testing.T) {
	forecast := client.Reminder{Kind: client.ReminderForecast, Value: 2, Key: "forecast:1:x"}
	read := func(sub client.Subscription) string {
		raw, err := viewJSONWith(sub, time.Now(), false, forecast)
		if err != nil {
			t.Fatal(err)
		}
		var view SubscriptionView
		if err := json.Unmarshal([]byte(raw), &view); err != nil {
			t.Fatal(err)
		}
		return view.Remind
	}
	quiet := client.Subscription{Nodes: []client.Node{{ID: 1}}}
	if got := read(quiet); got != client.ReminderForecast {
		t.Errorf("своих поводов нет, а напоминание %q — прогноз потерялся", got)
	}
	ending := quiet
	ending.ExpiresAt = time.Now().Add(20 * time.Hour).UTC().Format(time.RFC3339)
	if got := read(ending); got != "expiry" {
		t.Errorf("срок на исходе, а напоминание %q", got)
	}
}
