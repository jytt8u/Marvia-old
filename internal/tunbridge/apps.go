package tunbridge

import "context"

// Правила по программам: «Telegram всегда через VPN», «игра всегда мимо».
//
// Мост видит соединение как пакеты от адреса туннеля с каким-то портом, и
// чья это программа, по пакетам не узнать. Зато узнать может система: по
// порту, с которого открыто соединение, она называет процесс (на Windows —
// таблица соединений, см. cmd/marvia-windows/apps_windows.go). Мост отдаёт
// этот порт тому, кто умеет спросить систему, и получает ответ — куда вести.
//
// Правило программы сильнее правила сайта и российского списка: человек,
// сказавший «браузер — только через VPN», имеет в виду и российские сайты
// в нём тоже.

// AppRoute — куда вести соединение программы, открытое с порта srcPort.
// RouteAny — о программе правил нет.
type AppRoute func(srcPort uint16, udp bool) Route

type sourceKey struct{}

type source struct {
	port uint16
	udp  bool
}

func withSource(ctx context.Context, port uint16, udp bool) context.Context {
	return context.WithValue(ctx, sourceKey{}, source{port, udp})
}

func sourceOf(ctx context.Context) (source, bool) {
	s, ok := ctx.Value(sourceKey{}).(source)
	return s, ok
}

// WithApps добавляет к развилке правила по программам. Не развилка —
// возвращается как есть: без обхода решать по программе нечего.
func WithApps(d Dialer, apps AppRoute) Dialer {
	if s, ok := d.(*splitDialer); ok {
		s.apps = apps
	}
	return d
}
