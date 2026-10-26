package panel

import (
	"context"
	"errors"
	"time"
)

// Запасная нода.
//
// Адрес, который раздан покупателям, рано или поздно попадает в список
// блокировок. Запасная нода стоит наготове, синхронизируется с панелью и
// пускает покупателей, но в подписки не выходит, пока её основная жива: её
// адреса нет ни у кого, и заблокировать его заранее нечем. Основная легла,
// замолчала или её режут — запасная появляется в подписках сама, и клиенты
// переезжают на неё со следующим обновлением.

// standbySilence — сколько основная может молчать, прежде чем выйдет
// запасная. Десять минут: синхронизация идёт раз в полминуту, и десять
// пропусков подряд — уже не случайность сети.
const standbySilence = 10 * time.Minute

// ErrStandbyLoop — запасная для самой себя или для другой запасной.
var ErrStandbyLoop = errors.New("запасная нода — только для обычной ноды, не для себя и не для другой запасной")

// primaryDown — нужна ли запасная вместо основной.
func primaryDown(primary Node, h Health, now time.Time) bool {
	switch {
	case !primary.Enabled:
		// Выключил продавец или идёт обслуживание: покупателям нужен
		// другой адрес, и ровно для этого запасная и стоит.
		return true
	case primary.LastSeen == nil || now.Sub(*primary.LastSeen) > standbySilence:
		return true
	case h.Throttled() || h.Degraded():
		return true
	}
	return false
}

// withStandby убирает из подписки запасные ноды, чья основная в порядке.
func withStandby(nodes []Node, health map[int64]Health, now time.Time) []Node {
	byID := make(map[int64]Node, len(nodes))
	for _, n := range nodes {
		byID[n.ID] = n
	}
	out := nodes[:0:0]
	for _, n := range nodes {
		if n.StandbyFor != 0 {
			primary, ok := byID[n.StandbyFor]
			if ok && !primaryDown(primary, health[primary.ID], now) {
				continue
			}
		}
		out = append(out, n)
	}
	return out
}

// checkStandby проверяет, что основная — обычная нода, а не запасная и не
// сама эта.
func (s *Store) checkStandby(ctx context.Context, id, primary int64) error {
	if primary == 0 {
		return nil
	}
	if primary == id {
		return inputError{ErrStandbyLoop}
	}
	var of int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(standby_for, 0) FROM nodes WHERE id = ?`, primary).Scan(&of); err != nil {
		return inputError{errors.New("нет такой основной ноды")}
	}
	if of != 0 {
		return inputError{ErrStandbyLoop}
	}
	return nil
}
