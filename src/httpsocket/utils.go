package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"runtime"
	"strings"
	"time"
)

// самая полезная хелпер-функция в Go
func dieOnError(err error) {
	if err != nil {
		panic(err)
	}
}

// Соорудить http-клиент, умеющий таймаут
func MakeTimeoutingHttpClient(timeout time.Duration) *http.Client {
	timeoutFn := func(network, addr string) (net.Conn, error) {
		return net.DialTimeout(network, addr, timeout) // таймаут на подключение
	}

	transport := http.Transport{
		Dial: timeoutFn,
		ResponseHeaderTimeout: timeout, // таймаут на заголовки респонза
	}
	// таймаута на чтение ответа, похоже, нет

	client := http.Client{
		Transport: &transport,
	}
	return &client
}

// Форматирование стека текущего потока
func GetTraceback() string {
	tb := make([]byte, 4096)
	stb := string(tb[:runtime.Stack(tb, false)])
	lines := strings.Split(stb, "\n")
	for i := range lines {
		if strings.Contains(lines[i], "ServeHTTP") {
			// Пропускаем первые три строки - в них номер горутины и часть
			// стека этой функции
			return strings.Join(lines[2:i], "\n")
		}
	}
	return stb
}

// Хелпер-функция для ловли и логирования паник в горутинах,
// падение которых не должно завалить всё приложение
func simpleRecover() {
	if x := recover(); x != nil {
		stack := GetTraceback()
		errinfo := fmt.Sprintf("ERROR: PANIC: %s\n%s", x, stack)
		log.Printf("%s", errinfo)
	}
}

// Сериализовать JSON, паникуя при (совершенно уж неожиданной) ошибке
func MustMarshalJson(v interface{}) []byte {
	bs, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return bs
}

// Является ли указанный Content-Type родственным JSONу?
func IsJsonContentType(ct string) bool {
	if strings.HasPrefix(ct, "application/json") {
		return true
	}
	if strings.HasPrefix(ct, "text/json") {
		return true
	}
	return false
}
