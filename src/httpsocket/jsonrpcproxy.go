package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	urlmodule "net/url"
	"strings"
	"sync"
	"time"
)

// Принимаем JSON-RPC-запросы, превращаем их в HTTP-запросы, возвращаем HTTP-ответ как JSON-RPC-ответ.
//
// Детали:
//
// * Запрос
//
//     * method: "<HTTP_METHOD> <path+querystring>" (например "GET /mobileapi/catalogue/v5/")
//     * params: строка-тело POST-запроса
//     * id: строка (GUID)
//
// * Ответ
//
//     * id: поле id из соответствующего запроса. Если оно было пусто в запросе, ответ не высылается.
//     * http_status: код HTTP-ответа. Отсутствует, если не удалось сделать запрос (тогда будет заполнен error).
//     * http_content_type: Content-Type HTTP-ответа.
//     * result, error: смотри ниже.
//
//  Заполнение полей ответа зависит от вида HTTP-ответа апстрима.
//
//     1. HTTP-ответ имеет Content-Type: application-json и содержит поля result или error.
//        * result: поле result из ответа
//        * error: поле error из ответа
//     2. HTTP-ответ имеет Content-Type: application-json и имеет иной вид.
//        * result: тело HTTP-ответа (вложенный JSON).
//        * error: отсутствует.
//     3. HTTP-ответ имеет любой другой Content-Type.
//        * result: строка с телом ответа.
//        * error: отсутствует.
//     4. Не удалось получить HTTP-ответ (или даже выполнить HTTP-запрос)
//        * result: отсутствует.
//        * error: словарь {"code": code, "message": message}.

// Общие настройки проксирования
type ProxyParams struct {
	DefaultHost              string   // какой хост подставлять в проксируемые запросы, если клиент не указал хост
	WhitelistedUpstreamHosts []string // хосты, к которым разрешено проксировать запросы
	WhitelistedOrigins       []string // хосты, с которых разрешен доступ к вебсокету
}

// Стандартные и не очень коды ошибок JSON-RPC
const (
	ErrCodeInvalidMethod     = -32601
	ErrCodeInternalError     = -32603
	ErrCodeBadGateway        = -502 // не смогли спроксировать запрос
	ErrCodeGenericBadRequest = 400
)

type JsonWriter interface {
	WriteJSON(interface{}) error
}

// Клиент прокси-сервера
type ProxyClient struct {
	params          *ProxyParams
	originalRequest *http.Request // исходный HTTP-запрос от клиента
	xRealIp         string        // какой заголовок X-Real-IP проставлять в проксируемых запросах
	conn            JsonWriter    // куда следует писать JSON-ответ
	writeLock       sync.Mutex    // блокировка на запись в conn
	gotWriteError   bool          // поймали хотя бы одну ошибку при записи в conn?
	statCounter     *StatCounter
}

// Форматы запросов-ответов JSON-RPC

type JsonRpcRequest struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Id     interface{}     `json:"id"`
}

type JsonRpcResponse struct {
	Result               json.RawMessage `json:"result,omitempty"`
	Error                json.RawMessage `json:"error,omitempty"`
	HttpStatus           int             `json:"http_status,omitempty"`
	HttpContentType      string          `json:"http_content_type,omitempty"`
	UpstreamResponseTime float64         `json:"upstream_response_time_seconds,omitempty"`
	Id                   interface{}     `json:"id"`
}

type JsonRpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Ответ апстрима, отдаленно напоминающий JSON-RPC
type JsonRpcLikeResponse struct {
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

// Сформировать успешный ответ на запрос JSON-RPC
func (rq *JsonRpcRequest) MakeSimpleResponse(x interface{}) *JsonRpcResponse {
	jx := MustMarshalJson(x)
	return &JsonRpcResponse{
		Id:     rq.Id,
		Result: jx,
	}
}

var (
	FakeUpstreamResponse = fmt.Errorf("Fake upstream response")
)

// Обработать один HTTP-запрос
func (c *ProxyClient) HandleRpcRequest(rq *JsonRpcRequest) {
	defer c.statCounter.RequestFinished()
	defer simpleRecover()
	if c.handleSpecialMethod(rq) {
		return
	}

	methodAndUrl := strings.SplitN(rq.Method, " ", 2)
	if len(methodAndUrl) != 2 {
		c.SendError(rq, ErrCodeInvalidMethod, "malformed method")
		return
	}

	method := methodAndUrl[0]
	url := methodAndUrl[1]

	switch method {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS":
	default:
		c.SendError(rq, ErrCodeInvalidMethod, "unknown HTTP method "+method)
		return
	}

	c.LogDebugf("Request: %s %s", method, url)

	if strings.HasPrefix(url, "/") {
		if c.params.DefaultHost == "" {
			c.SendError(rq, ErrCodeInvalidMethod, "must specify protocol://host")
			return
		}
		url = "http://" + c.params.DefaultHost + "/" + url
	} else {
		if len(c.params.WhitelistedUpstreamHosts) > 0 {
			whitelisted := false
			u, err := urlmodule.Parse(url)
			if err != nil {
				c.SendError(rq, ErrCodeInvalidMethod, err.Error())
				return
			}
			for _, h := range c.params.WhitelistedUpstreamHosts {
				if h == u.Host {
					whitelisted = true
					break
				}
			}
			if !whitelisted {
				c.SendError(rq, ErrCodeInvalidMethod, "specified host not in whitelist")
				return
			}
		}
	}

	var rqBody io.Reader
	rqContentType := ""

	if method != "GET" && method != "HEAD" && len(rq.Params) > 0 {
		if rq.Params[0] == '"' { // JSON-строка
			rqContentType = "application/x-www-form-urlencoded"
			b := []byte(rq.Params)[1 : len(rq.Params)-1]
			rqBody = bytes.NewBuffer(b)
		} else { // произвольный JSON
			rqContentType = "application/json"
			rqBody = bytes.NewBuffer([]byte(rq.Params))
		}
	}

	httpRq, err := http.NewRequest(method, url, rqBody)
	if err != nil {
		c.SendError(rq, ErrCodeInternalError, err.Error())
		return
	}
	httpRq.Header.Add("X-Real-IP", c.xRealIp)
	httpRq.Header.Add("X-Request-ID", c.makeXRequestId(url))
	if rqContentType != "" {
		httpRq.Header.Add("Content-Type", rqContentType)
	}

	t0 := time.Now()
	var httpResp *http.Response

	if *fakeUpstreamResponseTimeMs > 0 {
		time.Sleep(time.Duration(*fakeUpstreamResponseTimeMs) * time.Millisecond)
		err = FakeUpstreamResponse
	} else {
		httpResp, err = httpClient.Do(httpRq)
	}

	dt := time.Since(t0)

	if err != nil {
		c.SendErrorWithTime(rq, ErrCodeBadGateway, err.Error(), dt.Seconds())
		return
	}
	defer httpResp.Body.Close()


	if rq.Id == nil { // запрос не требует ответа
		return
	}

	respContentType := httpResp.Header.Get("Content-Type")

	resp := &JsonRpcResponse{
		Id:                   rq.Id,
		HttpStatus:           httpResp.StatusCode,
		HttpContentType:      respContentType,
		UpstreamResponseTime: dt.Seconds(),
	}

	bs, err := ioutil.ReadAll(httpResp.Body)
	if err != nil {
		c.SendError(rq, ErrCodeBadGateway, "reading response: "+err.Error())
		return
	}

	// возможно, ответ апстрима напоминает JSON-RPC по структуре
	if IsJsonContentType(respContentType) {
		maybeRpcResponse := JsonRpcLikeResponse{}
		err := json.Unmarshal(bs, &maybeRpcResponse)
		if err == nil {
			if maybeRpcResponse.Result != nil || maybeRpcResponse.Error != nil {
				resp.Result = maybeRpcResponse.Result
				resp.Error = maybeRpcResponse.Error
			} else {
				// ответ содержит валидный JSON-объект, но без ключей result / error.
				// подставим его как есть в Result
				resp.Result = json.RawMessage(bs)
			}
		} else {
			// ответ не парсится как JSON-объект.
			// возможно это JSON-список, или еще что-нибудь такое.
			resp.Result = json.RawMessage(bs)
		}
	} else {
		// ответ апстрима содержит произвольную последовательность байт,
		// которую мы вернем в форме JSON-строки
		s := string(bs)
		resp.Result = json.RawMessage(MustMarshalJson(s))
	}
	c.Send(rq, resp)
}

// Обработать вызов встроенного служебного метода RPC, если такой указан в запросе.
//
// Возвращает true, если запрос был успешно обработан.
func (c *ProxyClient) handleSpecialMethod(rq *JsonRpcRequest) bool {
	switch rq.Method {
	case "httpsocket.setxrealip":
		var ip string
		err := json.Unmarshal(rq.Params, &ip)
		if err != nil {
			c.SendError(rq, ErrCodeGenericBadRequest, "params must be a string")
			return true
		}
		c.xRealIp = ip
		c.Send(rq, rq.MakeSimpleResponse("ok"))
	default:
		return false
	}
	return true
}

// Отправить клиенту сообщение об ошибке
func (c *ProxyClient) SendErrorWithTime(rq *JsonRpcRequest, errCode int, errMessage string, respTime float64) {
	if errMessage != FakeUpstreamResponse.Error() {
		c.LogWarnf("SendError(%s, %d, %s)", rq.Method, errCode, errMessage)
	}
	jerr := MustMarshalJson(&JsonRpcError{
		Code:    errCode,
		Message: errMessage,
	})
	c.Send(rq, &JsonRpcResponse{
		Error:                json.RawMessage(jerr),
		Id:                   rq.Id, // может быть пустым, но ошибку все равно нужно отправить
		UpstreamResponseTime: respTime,
	})
}

func (c *ProxyClient) SendError(rq *JsonRpcRequest, errCode int, errMessage string) {
	c.SendErrorWithTime(rq, errCode, errMessage, 0.0)
}

// Отправить сообщение клиенту
func (c *ProxyClient) Send(rq *JsonRpcRequest, x *JsonRpcResponse) {
	c.writeLock.Lock()
	defer c.writeLock.Unlock()

	err := c.conn.WriteJSON(x)
	if err != nil {
		c.gotWriteError = true
		if *logClientIoErrors {
			c.LogErrorf("Write: %s", err)
		}
	}
}

// Логирование ошибок при работе с этим клиентом
func (c *ProxyClient) LogErrorf(fmt string, params ...interface{}) {
	fmt = "ERROR [%s]: " + fmt
	params = append([]interface{}{c.originalRequest.RemoteAddr}, params...)
	log.Printf(fmt, params...)
}

// Логирование предупреждений при работе с этим клиентом
func (c *ProxyClient) LogWarnf(fmt string, params ...interface{}) {
	fmt = "WARN [%s]: " + fmt
	params = append([]interface{}{c.originalRequest.RemoteAddr}, params...)
	log.Printf(fmt, params...)
}

// Логирование информационных сообщений при работе с этим клиентом
func (c *ProxyClient) LogInfof(fmt string, params ...interface{}) {
	fmt = "INFO [%s]: " + fmt
	params = append([]interface{}{c.originalRequest.RemoteAddr}, params...)
	log.Printf(fmt, params...)
}

// Логирование отладочных сообщений при работе с этим клиентом
func (c *ProxyClient) LogDebugf(fmt string, params ...interface{}) {
	if !*debug {
		return
	}
	fmt = "DEBUG [%s]: " + fmt
	params = append([]interface{}{c.originalRequest.RemoteAddr}, params...)
	log.Printf(fmt, params...)
}

// Сформировать значение X-Request-ID для идентификации запроса
func (c *ProxyClient) makeXRequestId(url string) string {
	t := time.Now().Unix()
	url = strings.Split(url, "?")[0]
	return fmt.Sprintf("%d:%s::%s:ws-proxy", t, c.xRealIp, url)
}

// Общий для всех HTTP-клиент, через который идут проксируемые запросы
// TODO: отдельные таймауты на GET и на другие запросы
var (
	httpClient *http.Client
)

func initHttpClient(timeoutSeconds int) {
	httpClient = MakeTimeoutingHttpClient(time.Duration(timeoutSeconds) * time.Second)
}
