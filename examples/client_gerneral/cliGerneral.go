package main

import (
	"fmt"
	"time"

	modbus "github.com/thinkgos/gomodbus"
)

func main() {
	p := modbus.NewTCPClientProvider(":502")
	// p.BaudRate = 115200
	// p.DataBits = 8
	// p.Parity = "N"
	// p.StopBits = 1

	client := modbus.NewClient(p)
	client.LogMode(true)
	err := client.Connect()
	if err != nil {
		fmt.Println("connect", err)
		return
	}
	defer client.Close()

	fmt.Println("starting")
	for {
		_, err := client.ReadCoils(4, 0, 10)
		if err != nil {
			fmt.Println(err.Error())
		} else {
			//	fmt.Printf("ReadDiscreteInputs %#v\r\n", results)
		}
		time.Sleep(time.Second * 10)
	}
}