package mobile

import "strings"

// «Проверить всё» на телефоне — те же шаги, что в окне на компьютере
// (cmd/marvia-windows/diag.go), с теми же кодами и порогами, но без того,
// чего на Android не бывает.
//
// Человек с «не работает» видит зелёное «Подключено» и не знает, куда
// смотреть: есть ли у него интернет вообще, молчит ли нода, открываются ли
// сайты. Проверка идёт по шагам от сети к сайту, и у плохого шага — кнопка,
// которая что-то делает прямо здесь. Где нажать нечего, кнопки нет.
//
// Ядро отдаёт коды и числа, слова — у приложения, на его языке: иначе шаг
// говорил бы по-русски в английском интерфейсе. Тест сверяет, что у каждого
// кода есть строки в обоих словарях приложения и обработчик кнопки.
//
// Шагов из окна здесь нет два. «Браузер идёт через туннель» проверяет путь
// обычной программы через адаптер, а приложение Marvia из собственного
// туннеля исключено — открыть страницу «как браузер» ему нечем. «Помехи
// рядом» ищет второй VPN и системный прокси; второй VPN Android сам не
// пускает — он забирает интерфейс, и наш туннель просто выключается, — а
// прокси на телефоне не мешает туннелю: весь трафик и так в интерфейсе.

// diagSteps — шаги по порядку: от того, что не зависит от Marvia, к тому,
// что зависит. Приложение спрашивает их по одному и показывает каждый, как
// только он ответил: сайты проверяются секунды, и пустой экран всё это время
// выглядел бы зависанием.
var diagSteps = []string{"net", "dns", "node", "tunnel", "where", "leak", "sites"}

// diagActs — что приложение умеет сделать по плохому шагу. За каждым кодом
// стоит кнопка с настоящим действием; код без обработчика тест не пропустит.
var diagActs = []string{"connect", "reconnect", "nodes", "fragment", "dns"}

// Состояние шага. Off — проверять нечего: сайты через туннель, пока туннеля
// нет, — это не беда, а пропуск.
const (
	diagOK   = "ok"
	diagWarn = "warn"
	diagBad  = "bad"
	diagOff  = "off"
)

// diagSlowMS — отклик ноды, после которого стоит предложить другую: тот же
// порог, что в окне и у цвета отклика на главном экране.
const diagSlowMS = 300

// DiagSteps — коды шагов по порядку, через запятую. Строкой, а не списком:
// gomobile списков строк не переносит.
func DiagSteps() string { return strings.Join(diagSteps, ",") }

// diagStep — итог одного шага. Тип не экспортирован намеренно: наружу он
// уходит JSON, а gomobile попытался бы перенести и его поля-списки.
type diagStep struct {
	ID string `json:"id"`
	St string `json:"st"`
	// MS — время ответа; N из Of — сколько ответило из скольких.
	MS int64 `json:"ms,omitempty"`
	N  int   `json:"n,omitempty"`
	Of int   `json:"of,omitempty"`
	// IP и Country — под каким адресом и в какой стране видит интернет
	// через туннель. Это адрес ноды, а не человека.
	IP      string `json:"ip,omitempty"`
	Country string `json:"country,omitempty"`
	// Act — коды кнопок из diagActs; у хорошего шага их нет.
	Act []string `json:"act,omitempty"`
}

// judgeNet — интернет мимо VPN. Нет его — Marvia ничего не починит, и
// кнопки нет: только слова про Wi-Fi и мобильные данные.
func judgeNet(ok bool, ms int64) diagStep {
	if !ok {
		return diagStep{ID: "net", St: diagBad}
	}
	return diagStep{ID: "net", St: diagOK, MS: ms}
}

// judgeDNS — отвечают ли запросы имён так, как их получают приложения. С
// туннелем они идут к резолверу из настроек — его и предлагаем сменить. Без
// туннеля отвечает резолвер оператора, и лучшее, что можно сделать, —
// подключиться: имена пойдут через туннель.
func judgeDNS(ok, up bool, ms int64) diagStep {
	switch {
	case ok:
		return diagStep{ID: "dns", St: diagOK, MS: ms}
	case up:
		return diagStep{ID: "dns", St: diagBad, Act: []string{"dns"}}
	default:
		return diagStep{ID: "dns", St: diagBad, Act: []string{"connect"}}
	}
}

// judgeNode — отвечают ли ноды по TCP мимо туннеля. Молчат все — сменить не
// на что; молчит часть или отклик большой — на «Серверах» есть другая.
func judgeNode(answered, total int, best int64) diagStep {
	s := diagStep{ID: "node", N: answered, Of: total, MS: best}
	switch {
	case total == 0:
		s.St = diagOff
	case answered == 0:
		s.St = diagBad
		s.MS = 0
		if total > 1 {
			s.Act = []string{"nodes"}
		}
	case answered < total || best > diagSlowMS:
		s.St = diagWarn
		s.Act = []string{"nodes"}
	default:
		s.St = diagOK
	}
	return s
}

// judgeTunnel — поднят ли туннель. Не поднялся — попробовать ещё раз и, если
// дробление выключено, включить его: провайдер, режущий по имени, — самая
// частая причина, по которой нода жива, а хендшейк не проходит.
func judgeTunnel(state string, fragment bool) diagStep {
	s := diagStep{ID: "tunnel"}
	switch state {
	case "connected":
		s.St = diagOK
	case "connecting":
		s.St = diagWarn
	case "stalled":
		s.St = diagBad
		s.Act = []string{"reconnect", "nodes"}
	case "failed":
		s.St = diagBad
		s.Act = []string{"connect"}
		if !fragment {
			s.Act = append(s.Act, "fragment")
		}
	default:
		s.St = diagOff
		s.Act = []string{"connect"}
	}
	return s
}

// judgeWhere — под каким адресом человека видит интернет через туннель.
func judgeWhere(up bool, ip, country string) diagStep {
	switch {
	case !up:
		return diagStep{ID: "where", St: diagOff}
	case ip == "":
		return diagStep{ID: "where", St: diagBad, Act: []string{"nodes"}}
	default:
		return diagStep{ID: "where", St: diagOK, IP: ip, Country: country}
	}
}

// judgeLeak — не виден ли человек под своим адресом.
//
// В окне на компьютере сравниваются обычная программа и туннель: они обязаны
// совпасть. На телефоне наоборот. Само приложение из туннеля исключено (иначе
// соединение с нодой пошло бы в интерфейс, который к ней и ведёт), так что
// его обычный путь — это настоящий адрес телефона, а сайты, к которым ходят
// остальные приложения, видят адрес выхода туннеля. Совпали — сайты видят
// настоящий адрес: выход ноды стоит в той же сети, что телефон. Помогает
// другая нода.
//
// Ни одного из адресов в итоге нет: снимок экрана и отчёт человек пересылает
// продавцу, и настоящему адресу там не место.
func judgeLeak(up bool, tunnelIP, directIP string) diagStep {
	s := diagStep{ID: "leak"}
	switch {
	case !up:
		s.St = diagOff
	case tunnelIP == "" || directIP == "":
		s.St, s.Act = diagWarn, []string{"reconnect"}
	case tunnelIP == directIP:
		s.St, s.Act = diagBad, []string{"nodes"}
	default:
		s.St = diagOK
	}
	return s
}

// judgeSites — сколько главных сайтов открылось через туннель. Туннель
// поднят, а сайт не открылся — дело в выходе ноды, а не в пути до неё:
// помогает другая нода, а не дробление.
func judgeSites(up bool, opened, total int) diagStep {
	s := diagStep{ID: "sites", N: opened, Of: total}
	switch {
	case !up:
		s.St = diagOff
	case opened == total:
		s.St = diagOK
	case opened == 0:
		s.St = diagBad
		s.Act = []string{"nodes"}
	default:
		s.St = diagWarn
		s.Act = []string{"nodes"}
	}
	return s
}
