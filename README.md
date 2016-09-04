# httpsocket
HTTP-over-Websocket proxy

    git clone https://github.com/a-kr/httpsocket
    cd httpsocket
    make
    bin/httpsocket -debug --default-host api.lan -upstream-host-whitelist google.com,yandex.ru -origin-whitelist example.com
