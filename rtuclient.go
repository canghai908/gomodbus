package modbus

import (
	"encoding/binary"
	"fmt"
	"io"
	"time"
)

const (
	rtuExceptionSize = 5
)

// RTUClientProvider implements ClientProvider interface.
type RTUClientProvider struct {
	serialPort
	logs
	*pool // 请求池,所有RTU客户端共用一个请求池
}

// check RTUClientProvider implements underlying method
var _ ClientProvider = (*RTUClientProvider)(nil)

// 请求池,所有RTU客户端共用一个请求池
var rtuPool = newPool(rtuAduMaxSize)

// NewRTUClientProvider allocates and initializes a RTUClientProvider.
func NewRTUClientProvider(address string) *RTUClientProvider {
	p := &RTUClientProvider{
		logs: logs{newLogger(), 0},
		pool: rtuPool,
	}
	p.Address = address
	p.Timeout = SerialDefaultTimeout
	p.autoReconnect = SerialDefaultAutoReconnect
	return p
}

func (this *protocolFrame) encodeRTUFrame(slaveID byte, pdu ProtocolDataUnit) ([]byte, error) {
	length := len(pdu.Data) + 4
	if length > rtuAduMaxSize {
		return nil, fmt.Errorf("modbus: length of data '%v' must not be bigger than '%v'", length, rtuAduMaxSize)
	}
	requestAdu := this.adu[:0:length]
	requestAdu = append(requestAdu, slaveID, pdu.FuncCode)
	requestAdu = append(requestAdu, pdu.Data...)
	checksum := crc16(requestAdu)
	requestAdu = append(requestAdu, byte(checksum), byte(checksum>>8))
	return requestAdu, nil
}

// decode extracts slaveid and PDU from RTU frame and verify CRC.
func decodeRTUFrame(adu []byte) (uint8, []byte, error) {
	if len(adu) < rtuAduMinSize { // Minimum size (including address, funcCode and CRC)
		return 0, nil, fmt.Errorf("modbus: response length '%v' does not meet minimum '%v'", len(adu), rtuAduMinSize)
	}
	// Calculate checksum
	crc := crc16(adu[0 : len(adu)-2])
	expect := binary.LittleEndian.Uint16(adu[len(adu)-2:])
	if crc != expect {
		return 0, nil, fmt.Errorf("modbus: response crc '%x' does not match expected '%x'", expect, crc)
	}
	// slaveID & PDU but pass crc
	return adu[0], adu[1 : len(adu)-2], nil
}

// Send request to the remote server,it implements on SendRawFrame
func (this *RTUClientProvider) Send(slaveID byte, request ProtocolDataUnit) (ProtocolDataUnit, error) {
	var response ProtocolDataUnit

	frame := this.pool.get()
	defer this.pool.put(frame)

	aduRequest, err := frame.encodeRTUFrame(slaveID, request)
	if err != nil {
		return response, err
	}
	aduResponse, err := this.SendRawFrame(aduRequest)
	if err != nil {
		return response, err
	}
	rspSlaveID, pdu, err := decodeRTUFrame(aduResponse)
	if err != nil {
		return response, err
	}
	response = ProtocolDataUnit{pdu[0], pdu[1:]}
	if err = verify(slaveID, rspSlaveID, request, response); err != nil {
		return response, err
	}
	return response, nil
} //Function code & data

// SendPdu send pdu request to the remote server
func (this *RTUClientProvider) SendPdu(slaveID byte, pduRequest []byte) (pduResponse []byte, err error) {
	if len(pduRequest) < pduMinSize || len(pduRequest) > pduMaxSize {
		return nil, fmt.Errorf("modbus: pdu size '%v' must not be between '%v' and '%v'",
			len(pduRequest), pduMinSize, pduMaxSize)
	}

	frame := this.pool.get()
	defer this.pool.put(frame)

	request := ProtocolDataUnit{pduRequest[0], pduRequest[1:]}
	requestAdu, err := frame.encodeRTUFrame(slaveID, request)
	if err != nil {
		return nil, err
	}

	aduResponse, err := this.SendRawFrame(requestAdu)
	if err != nil {
		return nil, err
	}
	rspSlaveID, pdu, err := decodeRTUFrame(aduResponse)
	if err != nil {
		return nil, err
	}
	response := ProtocolDataUnit{pdu[0], pdu[1:]}
	if err = verify(slaveID, rspSlaveID, request, response); err != nil {
		return nil, err
	}
	//  PDU pass slaveID & crc
	return pdu, nil
}

// SendRawFrame send Adu frame
func (this *RTUClientProvider) SendRawFrame(aduRequest []byte) (aduResponse []byte, err error) {
	this.mu.Lock()
	defer this.mu.Unlock()

	// check  port is connected
	if !this.isConnected() {
		return nil, ErrClosedConnection
	}

	// Send the request
	this.Debug("modbus: sending % x", aduRequest)
	var tryCnt byte
	for {
		_, err = this.port.Write(aduRequest)
		if err == nil { // success
			break
		}
		if this.autoReconnect == 0 {
			return
		}
		for {
			err = this.connect()
			if err == nil {
				break
			}
			if tryCnt++; tryCnt >= this.autoReconnect {
				return
			}
		}
	}
	function := aduRequest[1]
	functionFail := aduRequest[1] & 0x80
	bytesToRead := calculateResponseLength(aduRequest)
	time.Sleep(this.calculateDelay(len(aduRequest) + bytesToRead))

	var n int
	var n1 int
	var data [rtuAduMaxSize]byte
	//We first read the minimum length and then read either the full package
	//or the error package, depending on the error status (byte 2 of the response)
	n, err = io.ReadAtLeast(this.port, data[:], rtuAduMinSize)
	if err != nil {
		return
	}

	switch {
	case data[1] == function:
		//if the function is correct
		//we read the rest of the bytes
		if n < bytesToRead {
			if bytesToRead > rtuAduMinSize && bytesToRead <= rtuAduMaxSize {
				if bytesToRead > n {
					n1, err = io.ReadFull(this.port, data[n:bytesToRead])
					n += n1
				}
			}
		}
	case data[1] == functionFail:
		//for error we need to read 5 bytes
		if n < rtuExceptionSize {
			n1, err = io.ReadFull(this.port, data[n:rtuExceptionSize])
		}
		n += n1
	default:
		err = fmt.Errorf("modbus: unknown function code % x", data[1])
	}
	if err != nil {
		return
	}
	aduResponse = data[:n]
	this.Debug("modbus: received % x\n", aduResponse)
	return
}

// calculateDelay roughly calculates time needed for the next frame.
// See MODBUS over Serial Line - Specification and Implementation Guide (page 13).
func (this *RTUClientProvider) calculateDelay(chars int) time.Duration {
	var characterDelay, frameDelay int // us

	if this.BaudRate <= 0 || this.BaudRate > 19200 {
		characterDelay = 750
		frameDelay = 1750
	} else {
		characterDelay = 15000000 / this.BaudRate
		frameDelay = 35000000 / this.BaudRate
	}
	return time.Duration(characterDelay*chars+frameDelay) * time.Microsecond
}

func calculateResponseLength(adu []byte) int {
	length := rtuAduMinSize
	switch adu[1] {
	case FuncCodeReadDiscreteInputs,
		FuncCodeReadCoils:
		count := int(binary.BigEndian.Uint16(adu[4:]))
		length += 1 + count/8
		if count%8 != 0 {
			length++
		}
	case FuncCodeReadInputRegisters,
		FuncCodeReadHoldingRegisters,
		FuncCodeReadWriteMultipleRegisters:
		count := int(binary.BigEndian.Uint16(adu[4:]))
		length += 1 + count*2
	case FuncCodeWriteSingleCoil,
		FuncCodeWriteMultipleCoils,
		FuncCodeWriteSingleRegister,
		FuncCodeWriteMultipleRegisters:
		length += 4
	case FuncCodeMaskWriteRegister:
		length += 6
	case FuncCodeReadFIFOQueue:
		// undetermined
	default:
	}
	return length
}

// helper

// verify confirms vaild data(including slaveID,funcCode,response data)
func verify(reqSlaveID, rspSlaveID uint8, reqPDU, rspPDU ProtocolDataUnit) error {
	switch {
	case reqSlaveID != rspSlaveID:
		// Check slaveid same
		return fmt.Errorf("modbus: response slave id '%v' does not match request '%v'", rspSlaveID, reqSlaveID)
	case rspPDU.FuncCode != reqPDU.FuncCode:
		// Check correct function code returned (exception)
		return responseError(rspPDU)
	case rspPDU.Data == nil || len(rspPDU.Data) == 0:
		// check Empty response
		return fmt.Errorf("modbus: response data is empty")
	}
	return nil
}
