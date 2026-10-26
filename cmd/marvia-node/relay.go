package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jytt8u/marvia/internal/connguard"
	"github.com/jytt8u/marvia/internal/proxyproto"
	"github.com/jytt8u/marvia/internal/relay"
)

// Вход каскада.
//
// Зарубежные адреса режут подсетями хостеров целиком, а при белых списках
// мобильный интернет пускает только к своим, российским. Маскировка от этого
// не спасает: блокируют сам адрес, а не то, что по нему ходит. Вход стоит в
// российском облаке — к нему соединение пропустят — и перекладывает байты на
// зарубежную ноду; от облака до неё связь уже обычная, межсерверная.
//
// Вход ничего не расшифровывает. TLS, REALITY и VP1 идут насквозь от
// покупателя до выхода, ключей у входа нет, список покупателей ему не
// отдаётся — панель говорит только, куда перекладывать. Изъятый вход выдаёт
// адрес выхода, который и так виден по его трафику, и больше ничего.
//
// Цена — лишний переход: задержка растёт на путь от входа до выхода, обычно
// на 20–50 мс из Москвы до Европы.

const (
	// relayDialTimeout — сколько ждём выход. Дольше покупатель всё равно
	// не ждёт: клиент на своей стороне уже сдался и пробует другую ноду.
	relayDialTimeout = 10 * time.Second

	// relayMaxConns — сколько соединений вход держит разом. На каждое — две
	// горутины и два буфера; предел держит память маленькой машины в рамках,
	// когда вход начнут простукивать сканеры.
	relayMaxConns = 8192
)

// runRelay — нода в режиме входа: синхронизируется с панелью, как обычная,
// но вместо протокола перекладывает соединения на выход.
// relayProbeEvery — как часто вход проверяет свой выход. Пять минут: блокировку
// продавец узнаёт быстрее покупателей, а лишнего стука в выход нет.
const relayProbeEvery = 5 * time.Minute

func runRelay(opts serverOptions) error {
	if opts.panelURL == "" {
		return errors.New("вход каскада работает только с панелью: укажи -panel")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Тот же путь, что у ноды: снимок, отзыв токена, выключение. Покупателей
	// панель входу не присылает, реестр остаётся пустым — нужен он только
	// циклу синхронизации.
	registry, _, panel, err := preparePanelUsers(ctx, opts)
	if err != nil {
		return err
	}
	go panel.Run(ctx, registry, panelSyncInterval, panelEvents())
	// Вход стоит в сети покупателей и первым видит, что выход заблокирован;
	// см. internal/nodesync/probe.go.
	go panel.ProbeExit(ctx, relayProbeEvery)

	ln, err := listenTCP(opts.listenAddr)
	if err != nil {
		return fmt.Errorf("прослушивание %s: %w", opts.listenAddr, err)
	}
	// Вход стоит в российском облаке на виду у всех, и простукивать его
	// начнут первым. Предел частоты — по адресу покупателя, и освобождать от
	// него некого: перед входом других входов нет. Число открытых у входа
	// и так держит relayMaxConns в serveRelay, он строже общего.
	guard := connguard.Listen(ln, connguard.Config{})
	panel.Guard(guard)
	ln = guard
	defer ln.Close()
	go func() {
		<-ctx.Done()
		log.Printf("завершаем работу")
		_ = ln.Close()
	}()

	log.Printf("marvia-node %s: вход каскада слушает %s", version, ln.Addr())
	if to := panel.RelayTo(); to != "" {
		log.Printf("перекладываю на %s", to)
	} else {
		log.Printf("панель пока не назвала выход: соединения закрываются, пока не назовёт")
	}
	return serveRelay(ln, panel.RelayTo)
}

// serveRelay принимает соединения и перекладывает каждое туда, куда
// указывает to в момент приёма. to читается на каждое соединение: продавец
// может сменить выход в панели, и новый доезжает за одну синхронизацию без
// перезапуска входа.
func serveRelay(ln net.Listener, to func() string) error {
	slots := make(chan struct{}, relayMaxConns)
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			log.Printf("accept: %v", err)
			continue
		}
		select {
		case slots <- struct{}{}:
		default:
			_ = conn.Close()
			continue
		}
		go func() {
			defer func() { <-slots }()
			forward(conn, to())
		}()
	}
}

// forward перекладывает одно соединение на выход.
//
// Первым уходит заголовок с адресом покупателя: выход считает устройства по
// адресам и без него увидел бы всех покупателей входа одним устройством.
// Ошибки не журналируются — в них адрес покупателя, а вход, как и нода, не
// ведёт журнала «кто откуда пришёл».
func forward(conn net.Conn, to string) {
	defer conn.Close()
	if to == "" {
		return
	}
	up, err := net.DialTimeout("tcp", to, relayDialTimeout)
	if err != nil {
		return
	}
	defer up.Close()
	if _, err := up.Write(proxyproto.Header(conn.RemoteAddr(), conn.LocalAddr())); err != nil {
		return
	}
	_ = relay.Bidirectional(conn, up)
}
