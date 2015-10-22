package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/getlantern/measured"

	"./utils"
)

var (
	help     = flag.Bool("help", false, "Get usage help")
	keyfile  = flag.String("key", "", "Private key file name")
	certfile = flag.String("cert", "", "Certificate file name")
	https    = flag.Bool("https", false, "Use TLS for client to proxy communication")
	addr     = flag.String("addr", ":8080", "Address to listen")
	token    = flag.String("token", "", "Lantern token")
	debug    = flag.Bool("debug", false, "Produce debug output")
)

func main() {
	var err error

	_ = flag.CommandLine.Parse(os.Args[1:])
	if *help {
		flag.Usage()
		return
	}

	var logLevel utils.LogLevel
	if *debug {
		logLevel = utils.DEBUG
	} else {
		logLevel = utils.ERROR
	}

	redisAddr := os.Getenv("REDIS_PRODUCTION_URL")
	if redisAddr == "" {
		redisAddr = "127.0.0.1:6379"
	}
	rp, err := utils.NewRedisReporter(redisAddr)
	if err != nil {
		fmt.Printf("Error connect to redis: %v\n", err)
	}
	measured.AddReporter(rp)
	measured.Start(20 * time.Second)
	defer measured.Stop()

	server := NewServer(*token, logLevel)
	if *https {
		err = server.ServeHTTPS(*addr, *keyfile, *certfile, nil)
	} else {
		err = server.ServeHTTP(*addr, nil)
	}
	if err != nil {
		fmt.Printf("Error serving: %v\n", err)
	}
}
