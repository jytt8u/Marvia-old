package client

import (
	"strconv"
	"time"
)

// Напоминание о конце доступа.
//
// Кончившаяся подписка без предупреждения выглядит поломкой: вчера работало,
// сегодня нет. Предупреждённый человек продлевает заранее и до поддержки не
// доходит вовсе — это дешевле и ему, и продавцу.
//
// Функция чистая: время передаётся снаружи, состояние «уже показывали» живёт
// у приложения. Ядру незачем знать, как и где приложение его хранит, а тесту —
// ждать трое суток.

// Виды напоминаний.
const (
	// ReminderExpiry — подходит срок; Value — сколько дней осталось.
	ReminderExpiry = "expiry"
	// ReminderTraffic — кончается трафик; Value — сколько процентов осталось.
	ReminderTraffic = "traffic"
)

// Пороги. За трое суток — чтобы успеть оплатить в выходные; за сутки —
// последний раз, когда напоминание ещё не опоздало. Десять процентов
// трафика — это обычно несколько дней жизни у того, кто смотрит видео.
const (
	expiryFirst  = 3 * 24 * time.Hour
	expiryLast   = 24 * time.Hour
	trafficShare = 10
)

// Reminder — о чём напомнить. Пустой Kind — не о чем.
type Reminder struct {
	Kind  string
	Value int64

	// Key — чем это напоминание отличается от прочих. Приложение помнит
	// последний показанный ключ и тот же самый второй раз не показывает.
	//
	// В ключ входит ступень («за 3 дня», «за 1 день»), а не число дней:
	// иначе «осталось 3», «осталось 2» считались бы разными и человека
	// дёргали бы каждый день. И входит срок: после продления срок другой,
	// значит и ключ другой, и следующий раз напомнят заново.
	Key string
}

// Reminder говорит, о чём напомнить на момент now.
//
// После продления напоминание снимается само: срок отодвинулся за порог, и
// напоминать не о чем. Кончившийся доступ — не напоминание, а отказ: его
// называет Allows.
//
// Если поводов два, отдаётся срочный: последний день важнее остатка
// трафика, а остаток трафика — важнее «через три дня».
func (s Subscription) Reminder(now time.Time) Reminder {
	until, limited := s.Until()
	left := until.Sub(now)
	expiring := limited && left > 0 && left <= expiryFirst

	if expiring && left <= expiryLast {
		return s.expiryReminder(until, left, 1)
	}
	if r := s.trafficReminder(); r.Kind != "" {
		return r
	}
	if expiring {
		return s.expiryReminder(until, left, 3)
	}
	return Reminder{}
}

func (s Subscription) expiryReminder(until time.Time, left time.Duration, step int) Reminder {
	// Дни округляем вверх: «осталось 0 дней» при живом доступе — неправда.
	days := int64((left + 24*time.Hour - 1) / (24 * time.Hour))
	return Reminder{
		Kind:  ReminderExpiry,
		Value: days,
		Key:   ReminderExpiry + ":" + strconv.Itoa(step) + ":" + until.UTC().Format(time.RFC3339),
	}
}

// trafficReminder — меньше десятой части трафика.
//
// Ключ — лимит и срок. Чем платим: если продавец обнулил расход, не тронув
// ни лимит, ни срок, следующий раз о трафике в этом периоде не напомнят.
// Различить «обнулили» и «ещё не дошли» по одной подписке нельзя, а
// напоминать каждый раз, как приложение открылось, хуже.
func (s Subscription) trafficReminder() Reminder {
	left := s.Remaining()
	if s.TrafficLimit <= 0 || left <= 0 || left*100 >= s.TrafficLimit*trafficShare {
		return Reminder{}
	}
	return Reminder{
		Kind:  ReminderTraffic,
		Value: left * 100 / s.TrafficLimit,
		Key:   ReminderTraffic + ":" + strconv.FormatInt(s.TrafficLimit, 10) + ":" + s.ExpiresAt,
	}
}

// ReminderForecast — трафик кончится раньше срока; Value — примерно через
// сколько дней при нынешнем расходе.
const ReminderForecast = "forecast"

// forecastDays — за сколько дней до конца трафика предупреждать. Трое суток —
// как и для срока: успеть оплатить в выходные.
const forecastDays = 3

// Forecast — прогноз по двум замерам остатка: был prevLeft в prevAt, стал
// cur в now. «Осталось 10%» человеку мало что говорит, а «при вашем
// расходе хватит примерно на 2 дня» — говорит.
//
// Прогноз только внутри одного периода — лимит и срок те же, — и только
// по промежутку от часа до недели: короче — случайный всплеск, длиннее —
// расход давно другой. Ключ — лимит и срок: в одном периоде прогноз
// показывают один раз.
func Forecast(prevLeft int64, prevLimit int64, prevExpires string, prevAt time.Time, cur Subscription, now time.Time) Reminder {
	left := cur.Remaining()
	if cur.TrafficLimit <= 0 || left <= 0 || prevLimit != cur.TrafficLimit || prevExpires != cur.ExpiresAt {
		return Reminder{}
	}
	spent, gap := prevLeft-left, now.Sub(prevAt)
	if spent <= 0 || gap < time.Hour || gap > 7*24*time.Hour {
		return Reminder{}
	}
	perDay := float64(spent) / gap.Hours() * 24
	days := float64(left) / perDay
	if days > forecastDays {
		return Reminder{}
	}
	// Вверх: «через 0 дней» при живом трафике — неправда.
	value := int64(days)
	if float64(value) < days {
		value++
	}
	return Reminder{
		Kind:  ReminderForecast,
		Value: value,
		Key:   ReminderForecast + ":" + strconv.FormatInt(cur.TrafficLimit, 10) + ":" + cur.ExpiresAt,
	}
}
