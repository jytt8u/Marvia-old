//go:build windows

package main

import (
	"fmt"
	"sync/atomic"
)

// Сообщения программы на языке окна.
//
// Часть того, что человек читает, приходит не из разметки, а отсюда: «нужны
// права администратора», «нода не отвечает», строки журнала. Английская кнопка
// с русской ошибкой под ней выглядит хуже, чем честно русское окно, поэтому
// язык знает и программа.
//
// При первом запуске окно предлагает выбрать язык; до выбора показывает язык
// системы. После этого выбор хранится в каталоге настроек приложения.
//
// Хранится в atomic, а не под замком: пишет его одна ручка, читают — сторож,
// журнал и обработчики, и заводить ради одного значения ещё один замок рядом с
// замком контроллера значит завести и порядок их взятия.
var uiLang atomic.Value

func init() { uiLang.Store("ru") }

// setUILang запоминает язык окна. Незнакомый оставляет прежний: соврать
// молчанием лучше, чем показать пустые строки.
func setUILang(lang string) {
	if _, known := messages[lang]; !known {
		return
	}
	uiLang.Store(lang)
}

// say отдаёт сообщение на языке окна.
//
// Ключ, а не строка: так недостающий перевод виден в словаре, а не всплывает
// у человека на экране.
func say(key string) string {
	lang, _ := uiLang.Load().(string)
	if m, ok := messages[lang][key]; ok {
		return m
	}
	// Русский — запасной: он заполнен всегда, потому что на нём пишут.
	if m, ok := messages["ru"][key]; ok {
		return m
	}
	return key
}

var messages = map[string]map[string]string{
	"ru": {
		"webViewTitle":    "Для окна Marvia нужен WebView2",
		"webViewMissing":  "Не найден Microsoft Edge WebView2 Runtime. VPN не запущен.\n\nУстановите Evergreen Runtime с официальной страницы Microsoft и снова откройте Marvia. Ключи и настройки сохранены.\n\nОткрыть официальную страницу загрузки?",
		"webViewFailed":   "Не удалось открыть окно Marvia. Проверьте или восстановите Microsoft Edge WebView2 Runtime и запустите приложение снова. VPN остановлен; ключи и настройки сохранены.",
		"noAccount":       "не задана ссылка доступа",
		"replaceKeyTitle": "Заменить ключ доступа?",
		"replaceKeyBody":  "Ссылка пришла извне — из браузера или другой программы. После замены трафик пойдёт через серверы того, кто её выдал. Соглашайтесь, только если ссылку прислал ваш продавец.",
		"logKeyKept":      "ссылка извне отклонена, ключ оставлен прежний",
		"needAdmin":       "нужны права администратора: без них Windows не даст создать сетевой адаптер",
		"adapterGone":     "адаптер VPN снова пропал из системы — его удаляет другая VPN-программа (например, Hiddify). Marvia отключилась, чтобы трафик не шёл мимо туннеля. Нажмите «Починить» — Marvia закроет мешающие программы и подключится снова",
		"conflictWarp":    "подключён Cloudflare WARP — второй VPN спорит с Marvia за маршруты. «Починить» отключит его до конца подключения Marvia",
		"conflictHiddify": "работает Hiddify — она может удалить адаптер Marvia и оставить свой прокси. «Починить» закроет её",
		"conflictVPN":     "работает ещё один VPN-клиент — два сразу мешают друг другу. «Починить» закроет его",
		"logAdapterLost":  "адаптер VPN пропал из системы — поднимаю туннель заново",
		"logDecoys":       "за сеанс отброшено поддельных пакетов zapret или GoodbyeDPI: %d — до сайтов они не дошли",
		"logDeadProxy":    "убран брошенный системный прокси %s: на нём никто не отвечал, и браузеры не открывали сайты",
		"logPaused":       "%s остановлен на время подключения, после отключения вернётся",
		"logPauseFailed":  "%s остановить не вышло: %v",
		"logResumed":      "%s снова запущен",
		"logResumeFailed": "%s запустить обратно не вышло: %v",
		"logCheckOK":      "проверка: сайты через туннель открываются",
		"logCheckFailed":  "проверка: страница не открылась (%v)",
		"checkPC":         "Нода работает, а трафик компьютера в туннель не попадает — мешает что-то на этом ПК: другой VPN, прокси или средство обхода. Нажмите «Починить»",
		"checkNode":       "Нода %s подключилась, но сайты через неё не открываются. Выберите другую на «Серверах»",
		"fixClosed":       "закрыто мешающих программ: %d",
		"fixProxy":        "убран брошенный прокси",
		"fixReconnect":    "подключаюсь заново",
		"fixNothing":      "мешающего не нашлось",
		"reportHeader":    "Отчёт Marvia для ПК %s — без ключей и адресов",
		"reportWindows":   "Windows %d.%d, сборка %d; права администратора: %s",
		"reportState":     "состояние: %s; нода: %s; транспорт: %s; отклик: %d мс",
		"reportReason":    "причина: %s",
		"reportCheck":     "проверка: %s",
		"reportSettings":  "российские сайты мимо: %s; домашняя сеть мимо: %s; kill switch: %s; DNS: %s, по HTTPS: %s; IPv6: %s; дробление: %s; только TCP: %s; отпечаток TLS: %s; MTU: %d; убирать помехи: %s",
		"reportFPSeller":  "как у продавца",
		"reportSites":     "сайтов мимо туннеля: %d; всегда через туннель: %d",
		"reportConflict":  "помеха: %s",
		"reportProxy":     "системный прокси: %s (%s)",
		"reportYes":       "да",
		"reportNo":        "нет",
		"clipboardBusy":   "буфер обмена занят другой программой — попробуйте ещё раз",
		"netNoTunnel":     "туннель не поднят — сначала подключитесь",
		"netWhereFailed":  "не удалось узнать адрес через туннель",
		"netSpeedFailed":  "замер скорости не прошёл — сервер замера не ответил через туннель",
		"proxyInEnv":      "прокси остался в переменных окружения — его убирает та программа, которая поставила",
		"nodeSilent":      "нода не отвечает — туннель поднят, но трафик через неё не идёт",
		"badRequest":      "не разобрал запрос",
		"accountLink":     "ссылка доступа",
		"privateKey":      "личный ключ",
		"bridge":          "сетевой мост",

		"logKeySaved":       "ключ доступа сохранён",
		"logProxyGone":      "системный прокси снят",
		"logTunnelUp":       "туннель поднят через %s",
		"logTunnelDown":     "туннель убран, маршруты сняты",
		"logNoConnect":      "не подключилось: %v",
		"logCleanup":        "при уборке: %v",
		"logConn":           "соединение: %v",
		"logNodeBack":       "нода снова отвечает",
		"logChosen":         "выбрана нода %s",
		"notConnected":      "туннель не поднят",
		"logMoved":          "переехали на %s",
		"logNodeSilent":     "нода не отвечает на %d проверки подряд: %v",
		"logAdapter":        "создаю сетевой адаптер и настраиваю маршруты",
		"logNodePicked":     "выбрана нода %s, задержка %d мс",
		"autostartFailed":   "планировщик отказал: задачу может поставить только администратор",
		"logAutostartOn":    "запуск вместе с Windows включён",
		"logAutostartOff":   "запуск вместе с Windows выключен",
		"bypassForeign":     "этот список даёт только панель Marvia, а подписка чужая",
		"logBypassCount":    "российских подсетей мимо туннеля: %d",
		"logBypassFailed":   "российский список не скачался: %v",
		"logBypassOff":      "российские сайты снова идут через туннель",
		"logKillOn":         "kill switch стоит: мимо туннеля закрыто всё, кроме самого Marvia",
		"logKillOff":        "kill switch снят: без туннеля трафик снова идёт напрямую",
		"logKillFailed":     "kill switch не встал, туннель работает без него: %v",
		"sellerNoLink":      "продавец не оставил такой ссылки",
		"trayRemindTitle":   "Marvia: пора продлить",
		"trayRemindExpiry":  "До конца доступа дней: %d. Продлите заранее, чтобы VPN не встал. Нажмите, чтобы открыть «Продлить».",
		"trayRemindTraffic": "Осталось %d%% трафика. Продлите заранее, чтобы VPN не встал. Нажмите, чтобы открыть «Продлить».",
		"sellerBadLink":     "ссылка продавца не прошла проверку и не открыта",
		"logSellerBadLink":  "ссылка продавца отвергнута: %v",
		"dnsBad":            "нужен адрес IPv4 вида 1.1.1.1",
		"mtuRange":          "MTU — от 1280 до 1500 или пусто, чтобы вернуть по умолчанию",
		"dnsLocal":          "это адрес локальной сети: из туннеля он не виден",
		"siteBad":           "не похоже на имя сайта: %s",
		"appBad":            "программа — имя файла вроде telegram.exe, без пути; не больше 50",
		"tipConnected":      "Подключено",
		"tipConnecting":     "Подключаюсь…",
		"tipStalled":        "Нода не отвечает",
		"tipFailed":         "Не подключилось",
		"tipOff":            "Отключено",
		"tipPing":           "%d мс",
		"siteTooMany":       "в списке больше тысячи сайтов — столько руками не ведут",
		"logNamesSecure":    "имена идут к %s по HTTPS: нода видит только соединение с резолвером",
		"logNamesPlain":     "имена идут открытым DNS через туннель: нода их видит",
		"keyGone":           "такого ключа уже нет в списке",
		"logKeyUsed":        "рабочий ключ — %s",
		"logKeyRemoved":     "ключ %s убран",
	},
	"en": {
		"webViewTitle":    "Marvia needs WebView2 to display its window",
		"webViewMissing":  "Microsoft Edge WebView2 Runtime was not found. The VPN has not started.\n\nInstall the Evergreen Runtime from the official Microsoft page and open Marvia again. Your keys and settings are kept.\n\nOpen the official download page?",
		"webViewFailed":   "Marvia could not open its window. Check or repair Microsoft Edge WebView2 Runtime and start the app again. The VPN has stopped; your keys and settings are kept.",
		"noAccount":       "no access key set",
		"replaceKeyTitle": "Replace the access key?",
		"replaceKeyBody":  "This link came from outside — a browser or another program. After replacing it, your traffic goes through the servers of whoever issued it. Agree only if the link came from your seller.",
		"logKeyKept":      "outside link declined, the old key is kept",
		"needAdmin":       "administrator rights are required: without them Windows will not create a network adapter",
		"adapterGone":     "the VPN adapter disappeared again — another VPN program (for example, Hiddify) keeps removing it. Marvia disconnected so traffic does not bypass the tunnel. Press “Fix” — Marvia will close the programs in the way and connect again",
		"conflictWarp":    "Cloudflare WARP is connected — a second VPN fights Marvia over routes. “Fix” disconnects it until Marvia disconnects",
		"conflictHiddify": "Hiddify is running — it can remove the Marvia adapter and leave its proxy behind. “Fix” closes it",
		"conflictVPN":     "another VPN client is running — two at once get in each other's way. “Fix” closes it",
		"logAdapterLost":  "the VPN adapter disappeared from the system — bringing the tunnel up again",
		"logDecoys":       "fake packets from zapret or GoodbyeDPI dropped this session: %d — none reached the sites",
		"logDeadProxy":    "removed an abandoned system proxy %s: nothing answered on it, and browsers could not open sites",
		"logPaused":       "%s paused for the connection, it comes back after you disconnect",
		"logPauseFailed":  "could not pause %s: %v",
		"logResumed":      "%s started again",
		"logResumeFailed": "could not start %s again: %v",
		"logCheckOK":      "check: sites open through the tunnel",
		"logCheckFailed":  "check: the page did not open (%v)",
		"checkPC":         "The node works, but this PC's traffic does not reach the tunnel — something on this PC is in the way: another VPN, a proxy or a DPI bypass tool. Press “Fix”",
		"checkNode":       "Node %s connected, but sites do not open through it. Pick another one on Servers",
		"fixClosed":       "programs closed: %d",
		"fixProxy":        "abandoned proxy removed",
		"fixReconnect":    "connecting again",
		"fixNothing":      "nothing in the way found",
		"reportHeader":    "Marvia for PC %s report — no keys or addresses",
		"reportWindows":   "Windows %d.%d, build %d; administrator: %s",
		"reportState":     "state: %s; node: %s; transport: %s; ping: %d ms",
		"reportReason":    "reason: %s",
		"reportCheck":     "check: %s",
		"reportSettings":  "Russian sites around: %s; home network around: %s; kill switch: %s; DNS: %s, over HTTPS: %s; IPv6: %s; split hello: %s; TCP only: %s; TLS fingerprint: %s; MTU: %d; clear interference: %s",
		"reportFPSeller":  "seller's",
		"reportSites":     "sites around the tunnel: %d; always through the tunnel: %d",
		"reportConflict":  "interference: %s",
		"reportProxy":     "system proxy: %s (%s)",
		"reportYes":       "yes",
		"reportNo":        "no",
		"clipboardBusy":   "the clipboard is busy with another program — try again",
		"netNoTunnel":     "the tunnel is not up — connect first",
		"netWhereFailed":  "could not learn the address through the tunnel",
		"netSpeedFailed":  "the speed test failed — the test server did not answer through the tunnel",
		"proxyInEnv":      "the proxy is still in environment variables — only the program that set it can remove it",
		"nodeSilent":      "the node is not responding — the tunnel is up, but no traffic goes through it",
		"badRequest":      "could not parse the request",
		"accountLink":     "access key",
		"privateKey":      "private key",
		"bridge":          "network bridge",

		"logKeySaved":       "access key saved",
		"logProxyGone":      "system proxy removed",
		"logTunnelUp":       "tunnel up through %s",
		"logTunnelDown":     "tunnel removed, routes cleared",
		"logNoConnect":      "could not connect: %v",
		"logCleanup":        "while cleaning up: %v",
		"logConn":           "connection: %v",
		"logNodeBack":       "the node responds again",
		"logChosen":         "node %s chosen",
		"notConnected":      "the tunnel is not up",
		"logMoved":          "moved to %s",
		"logNodeSilent":     "the node failed %d checks in a row: %v",
		"logAdapter":        "creating the network adapter and setting up routes",
		"logNodePicked":     "picked node %s, latency %d ms",
		"autostartFailed":   "the task scheduler refused: only an administrator can create the task",
		"logAutostartOn":    "start with Windows enabled",
		"logAutostartOff":   "start with Windows disabled",
		"bypassForeign":     "only a Marvia panel provides this list, and this subscription is not one",
		"logBypassCount":    "Russian subnets around the tunnel: %d",
		"logBypassFailed":   "the Russian list did not download: %v",
		"logBypassOff":      "Russian sites go through the tunnel again",
		"logKillOn":         "kill switch is on: everything outside the tunnel is closed except Marvia itself",
		"logKillOff":        "kill switch removed: without the tunnel traffic goes direct again",
		"logKillFailed":     "kill switch did not start, the tunnel runs without it: %v",
		"sellerNoLink":      "your seller left no such link",
		"trayRemindTitle":   "Marvia: time to renew",
		"trayRemindExpiry":  "Days of access left: %d. Renew in advance so the VPN keeps working. Click to open Renew.",
		"trayRemindTraffic": "%d%% of traffic left. Renew in advance so the VPN keeps working. Click to open Renew.",
		"sellerBadLink":     "the seller's link failed the check and was not opened",
		"logSellerBadLink":  "seller's link rejected: %v",
		"dnsBad":            "an IPv4 address like 1.1.1.1 is needed",
		"mtuRange":          "MTU must be 1280–1500, or empty for the default",
		"dnsLocal":          "this is a local network address: it is not visible from the tunnel",
		"siteBad":           "this does not look like a site name: %s",
		"appBad":            "a program is a file name like telegram.exe, without a path; 50 at most",
		"tipConnected":      "Connected",
		"tipConnecting":     "Connecting…",
		"tipStalled":        "Node not responding",
		"tipFailed":         "Could not connect",
		"tipOff":            "Disconnected",
		"tipPing":           "%d ms",
		"siteTooMany":       "more than a thousand sites in the list — nobody keeps that many by hand",
		"logNamesSecure":    "name lookups go to %s over HTTPS: the node only sees a connection to the resolver",
		"logNamesPlain":     "name lookups go as plain DNS through the tunnel: the node can read them",
		"keyGone":           "this key is no longer in the list",
		"logKeyUsed":        "active key: %s",
		"logKeyRemoved":     "key %s removed",
	},
}

// sayf собирает сообщение с подстановками.
//
// sprintf вынесен в переменную намеренно. Строка формата приходит из словаря
// выше, а не из кода, и go vet справедливо требует постоянного формата:
// подставить туда чужое значение — известная дыра. Здесь формат свой, лежит
// в таблице десятью строками выше и меняется только вместе с ней, поэтому
// проверять действительно нечего. Обращение через переменную говорит это vet,
// а комментарий — тому, кто будет читать.
var sprintf = fmt.Sprintf

func sayf(key string, args ...any) string { return sprintf(say(key), args...) }
