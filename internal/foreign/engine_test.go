package foreign

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/infra/conf/serial"

	// Серверная сторона — только в тестах: в приложение она не попадает.
	_ "github.com/xtls/xray-core/app/proxyman/inbound"
	_ "github.com/xtls/xray-core/proxy/freedom"
	_ "github.com/xtls/xray-core/proxy/vless/inbound"
	_ "github.com/xtls/xray-core/proxy/vmess/inbound"

	"github.com/jytt8u/marvia/internal/vp1"
)

// freePort берёт у системы порт, свободный и для TCP, и для UDP.
//
// Shadowsocks слушает оба на одном номере. Свободный TCP-порт на Windows
// бывает занят для UDP — система держит диапазоны под свои службы, — и
// тест падал «bind: access permissions» раз в несколько прогонов CI.
func freePort(t *testing.T) int {
	t.Helper()
	for range 50 {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := ln.Addr().(*net.TCPAddr).Port
		udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
		ln.Close()
		if err == nil {
			udp.Close()
			return port
		}
	}
	t.Fatal("нет порта, свободного и для TCP, и для UDP")
	return 0
}

// selfSigned выпускает сертификат на 127.0.0.1 для серверов с TLS.
func selfSigned(t *testing.T) (certPEM, keyPEM string) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		DNSNames: []string{"test.local"}, IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, _ := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	kder, _ := x509.MarshalECPrivateKey(key)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}))
}

// server поднимает Xray-сервер с одним входом и выходом прямо в сеть.
func server(t *testing.T, inbound map[string]any) *core.Instance {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{
		"log":      map[string]any{"loglevel": "none"},
		"inbounds": []any{inbound},
		// Цели тестов — на 127.0.0.1, а Xray с июля 2026 года по умолчанию не
		// пускает серверы VLESS, VMess и Trojan в частные сети (защита от
		// SSRF). Тестовому серверу это разрешаем явно.
		"outbounds": []any{map[string]any{"protocol": "freedom",
			"settings": map[string]any{"finalRules": []any{map[string]any{"action": "allow"}}}}},
	})
	pb, err := serial.LoadJSONConfig(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("настройки сервера: %v", err)
	}
	inst, err := core.New(pb)
	if err != nil {
		t.Fatalf("сервер: %v", err)
	}
	if err := inst.Start(); err != nil {
		t.Fatalf("сервер: %v", err)
	}
	t.Cleanup(func() { _ = inst.Close() })
	return inst
}

// echo — цель, до которой клиент дойдёт через ноду.
func echo(t *testing.T) vp1.Address {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	a, _ := vp1.AddressFromHostPort("127.0.0.1", uint16(ln.Addr().(*net.TCPAddr).Port))
	return a
}

// TestEveryProtocolReachesTheTargetThroughItsLink: ссылка каждого вида,
// разобранная клиентом, доводит поток до цели через настоящий сервер Xray —
// ровно так, как это делает телефон. Проверяется не разбор, а путь целиком,
// и дважды: второй раз с дроблением приветствия — сервер обязан принять
// разрезанное так же, как целое.
func TestEveryProtocolReachesTheTargetThroughItsLink(t *testing.T) {
	cert, key := selfSigned(t)
	tlsServer := map[string]any{"security": "tls", "tlsSettings": map[string]any{
		"certificates": []any{map[string]any{"certificate": strings.Split(cert, "\n"), "key": strings.Split(key, "\n")}},
	}}
	const uuid = "b831381d-6324-4d53-ad4f-8cda48b30811"

	cases := []struct {
		name    string
		inbound func(port int) map[string]any
		link    func(port int) string
	}{
		{"vless поверх ws", func(p int) map[string]any {
			return map[string]any{"port": p, "listen": "127.0.0.1", "protocol": "vless",
				"settings":       map[string]any{"clients": []any{map[string]any{"id": uuid}}, "decryption": "none"},
				"streamSettings": map[string]any{"network": "ws", "wsSettings": map[string]any{"path": "/v"}}}
		}, func(p int) string {
			return "vless://" + uuid + "@127.0.0.1:" + strconv.Itoa(p) + "?type=ws&path=%2Fv&security=none#VLESS"
		}},
		{"vless поверх tls", func(p int) map[string]any {
			s := map[string]any{"network": "raw"}
			for k, v := range tlsServer {
				s[k] = v
			}
			return map[string]any{"port": p, "listen": "127.0.0.1", "protocol": "vless",
				"settings":       map[string]any{"clients": []any{map[string]any{"id": uuid}}, "decryption": "none"},
				"streamSettings": s}
		}, func(p int) string {
			return "vless://" + uuid + "@127.0.0.1:" + strconv.Itoa(p) + "?security=tls&sni=test.local&pcs=" + pinOf(cert)
		}},
		{"vmess", func(p int) map[string]any {
			return map[string]any{"port": p, "listen": "127.0.0.1", "protocol": "vmess",
				"settings": map[string]any{"clients": []any{map[string]any{"id": uuid}}}}
		}, func(p int) string {
			body, _ := json.Marshal(map[string]any{"v": "2", "ps": "VMess", "add": "127.0.0.1", "port": strconv.Itoa(p), "id": uuid, "aid": "0", "net": "tcp", "type": "none", "tls": ""})
			return "vmess://" + b64(body)
		}},
		{"trojan", func(p int) map[string]any {
			s := map[string]any{"network": "raw"}
			for k, v := range tlsServer {
				s[k] = v
			}
			return map[string]any{"port": p, "listen": "127.0.0.1", "protocol": "trojan",
				"settings":       map[string]any{"clients": []any{map[string]any{"password": "секрет-трояна"}}},
				"streamSettings": s}
		}, func(p int) string {
			return "trojan://%D1%81%D0%B5%D0%BA%D1%80%D0%B5%D1%82-%D1%82%D1%80%D0%BE%D1%8F%D0%BD%D0%B0@127.0.0.1:" + strconv.Itoa(p) + "?sni=test.local&pcs=" + pinOf(cert) + "#Trojan"
		}},
		{"shadowsocks", func(p int) map[string]any {
			return map[string]any{"port": p, "listen": "127.0.0.1", "protocol": "shadowsocks",
				"settings": map[string]any{"method": "chacha20-ietf-poly1305", "password": "пароль", "network": "tcp,udp"}}
		}, func(p int) string {
			return "ss://" + b64([]byte("chacha20-ietf-poly1305:пароль")) + "@127.0.0.1:" + strconv.Itoa(p) + "#SS"
		}},
	}

	target := echo(t)
	for _, split := range []bool{false, true} {
		for _, c := range cases {
			name := c.name
			if split {
				name += " с дроблением"
			}
			t.Run(name, func(t *testing.T) {
				var cuts atomic.Int32
				fragmentedDial = func() { cuts.Add(1) }
				defer func() { fragmentedDial = func() {} }()
				reach(t, c.inbound, c.link, target, Options{Fragment: split})
				// Резать должен настоящий дозвон движка: экземпляр Xray обязан
				// дойти до него в контексте, иначе настройка — пустое обещание.
				if split && cuts.Load() == 0 {
					t.Fatal("движок с дроблением ни разу не резал")
				}
				if !split && cuts.Load() != 0 {
					t.Fatal("движок без дробления резал")
				}
			})
		}
	}
}

// reach поднимает сервер, разбирает ссылку и гоняет через неё эхо.
func reach(t *testing.T, inbound func(int) map[string]any, link func(int) string, target vp1.Address, opts Options) {
	t.Helper()
	port := freePort(t)
	server(t, inbound(port))
	l, err := Parse(link(port))
	if err != nil {
		t.Fatalf("разбор: %v", err)
	}
	e, err := StartWith(l, opts)
	if err != nil {
		t.Fatalf("движок: %v", err)
	}
	defer e.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := e.DialTarget(ctx, target)
	if err != nil {
		t.Fatalf("дозвон: %v", err)
	}
	defer conn.Close()
	msg := []byte("привет через " + l.Protocol)
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write(msg); err != nil {
		t.Fatalf("запись: %v", err)
	}
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("ответ не дошёл: %v", err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatalf("ответ %q, ожидался %q", got, msg)
	}
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// pinOf — отпечаток сертификата, как его пишут в ссылки: sha256 в hex.
func pinOf(certPEM string) string {
	b, _ := pem.Decode([]byte(certPEM))
	sum := sha256.Sum256(b.Bytes)
	return hex.EncodeToString(sum[:])
}

// TestDatagramsGoThroughTheNode: датаграммы — запросы имён, QUIC — доходят
// через ноду и возвращаются с границами: без них телефон не разрешит ни
// одного имени.
func TestDatagramsGoThroughTheNode(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = pc.WriteTo(append([]byte("эхо:"), buf[:n]...), from)
		}
	}()
	link, _ := vlessNode(t, "UDP")
	l, _ := Parse(link)
	e, err := Start(l)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	target, _ := vp1.AddressFromHostPort("127.0.0.1", uint16(pc.LocalAddr().(*net.UDPAddr).Port))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := e.DialDatagrams(ctx, target)
	if err != nil {
		t.Fatalf("дозвон: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	for _, msg := range []string{"раз", "два"} {
		if _, err := conn.Write([]byte(msg)); err != nil {
			t.Fatalf("запись: %v", err)
		}
		buf := make([]byte, 2048)
		n, err := conn.Read(buf)
		if err != nil {
			t.Fatalf("ответ на %q не пришёл: %v", msg, err)
		}
		if got := string(buf[:n]); got != "эхо:"+msg {
			t.Fatalf("ответ %q, ожидался %q", got, "эхо:"+msg)
		}
	}
}

// TestConnectionOutlivesTheDialContext: срок дозвона ограничивает дозвон, а не
// соединение. Мост телефона отменяет контекст сразу, как только поток
// открыт, — так и должно быть, а Xray привязывал к нему всё соединение, и
// оно умирало сразу после открытия: туннель был поднят, а сайты не шли.
func TestConnectionOutlivesTheDialContext(t *testing.T) {
	target := echo(t)
	link, _ := vlessNode(t, "Срок")
	l, _ := Parse(link)
	e, err := Start(l)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	conn, err := e.DialTarget(ctx, target)
	cancel() // ровно как tunbridge: дозвон кончился — срок больше не нужен
	if err != nil {
		t.Fatalf("дозвон: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte("живо")); err != nil {
		t.Fatalf("запись: %v", err)
	}
	got := make([]byte, len("живо"))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("соединение умерло вместе со сроком дозвона: %v", err)
	}
}
