it:
	go get github.com/coocood/freecache
	go get github.com/fasthttp/router
	go get github.com/go-sql-driver/mysql
	go get github.com/jmoiron/sqlx
	go get github.com/mattn/go-sqlite3
	go get github.com/olebedev/config
	go get github.com/oschwald/geoip2-golang
	go get github.com/valyala/fasthttp
	go build mirrorlist_updater.go
	go build mirrorlist.go
