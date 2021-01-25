package modbus

import (
	"encoding/binary"
	"fmt"
	//"io"
	//"strconv"
	"time"
)

const (
	rtuExceptionSize = 5
)

// RTUClientProvider implements ClientProvider interface.
type RTUClientProvider struct {
	serialPort
	logger
	*pool
}

// check RTUClientProvider implements the interface ClientProvider underlying method
var _ ClientProvider = (*RTUClientProvider)(nil)

// request pool, all RTU client use this pool
var rtuPool = newPool(rtuAduMaxSize)

// NewRTUClientProvider allocates and initializes a RTUClientProvider.
// it will use default /dev/ttyS0 19200 8 1 N and timeout 1000
func NewRTUClientProvider(opts ...ClientProviderOption) *RTUClientProvider {
	p := &RTUClientProvider{
		logger: newLogger("modbusRTUMaster => "),
		pool:   rtuPool,
	}
	p.autoReconnect = SerialDefaultAutoReconnect
	for _, opt := range opts {
		opt(p)
	}
	return p
}

func (sf *protocolFrame) encodeRTUFrame(slaveID byte, pdu ProtocolDataUnit) ([]byte, error) {
	length := len(pdu.Data) + 4
	if length > rtuAduMaxSize {
		return nil, fmt.Errorf("modbus: length of data '%v' must not be bigger than '%v'", length, rtuAduMaxSize)
	}
	requestAdu := sf.adu[:0:length]
	requestAdu = append(requestAdu, slaveID, pdu.FuncCode)
	requestAdu = append(requestAdu, pdu.Data...)
	checksum := CRC16(requestAdu)
	requestAdu = append(requestAdu, byte(checksum), byte(checksum>>8))
	return requestAdu, nil
}

// decode extracts slaveID and PDU from RTU frame and verify CRC.
func decodeRTUFrame(adu []byte) (uint8, []byte, error) {
	if len(adu) < rtuAduMinSize { // Minimum size (including address, funcCode and CRC)
		return 0, nil, fmt.Errorf("modbus: response length '%v' does not meet minimum '%v'", len(adu), rtuAduMinSize)
	}
	// Calculate checksum
	crc, expect := CRC16(adu[:len(adu)-2]), binary.LittleEndian.Uint16(adu[len(adu)-2:])
	if crc != expect {
		return 0, nil, fmt.Errorf("modbus: response crc '%x' does not match expected '%x'", expect, crc)
	}
	// slaveID & PDU but pass crc
	return adu[0], adu[1 : len(adu)-2], nil
}

// Send request to the remote server, it implements on SendRawFrame
func (sf *RTUClientProvider) Send(slaveID byte, request ProtocolDataUnit) (ProtocolDataUnit, error) {
	var response ProtocolDataUnit

	frame := sf.pool.get()
	defer sf.pool.put(frame)

	aduRequest, err := frame.encodeRTUFrame(slaveID, request)
	if err != nil {
		return response, err
	}
	aduResponse, err := sf.SendRawFrame(aduRequest)
	if err != nil {
		return response, err
	}
	rspSlaveID, pdu, err := decodeRTUFrame(aduResponse)
	if err != nil {
		return response, err
	}
	response = ProtocolDataUnit{pdu[0], pdu[1:]}
	err = verify(slaveID, rspSlaveID, request, response)
	return response, err
}

// SendPdu send pdu request to the remote server
func (sf *RTUClientProvider) SendPdu(slaveID byte, pduRequest []byte) ([]byte, error) {
	if len(pduRequest) < pduMinSize || len(pduRequest) > pduMaxSize {
		return nil, fmt.Errorf("modbus: pdu size '%v' must not be between '%v' and '%v'",
			len(pduRequest), pduMinSize, pduMaxSize)
	}

	frame := sf.pool.get()
	defer sf.pool.put(frame)

	request := ProtocolDataUnit{pduRequest[0], pduRequest[1:]}
	requestAdu, err := frame.encodeRTUFrame(slaveID, request)
	if err != nil {
		return nil, err
	}
	aduResponse, err := sf.SendRawFrame(requestAdu)
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
func (sf *RTUClientProvider) SendRawFrame(aduRequest []byte) (aduResponse []byte, err error) {
	sf.mu.Lock()
	defer sf.mu.Unlock()

	// check  port is connected
	if !sf.isConnected() {
		return nil, ErrClosedConnection
	}

	// Send the request
	sf.Debug("sending [% x]", aduRequest)
	_, err = sf.port.Write(aduRequest)
	if err != nil {
		sf.Error("222", err)
	}
	data := make([]byte, 1024)
	time.Sleep(1000 * time.Millisecond)
	n, err := sf.port.Read(data)
	if err != nil {
		return nil, err
	}
	aduResponse = data[:n]
	sf.Debug("receive [% x]", data[:n])
	return aduResponse, nil
}

// calculateDelay roughly calculates time needed for the next frame.
// See MODBUS over Serial Line - Specification and Implementation Guide (page 13).
func (sf *RTUClientProvider) calculateDelay(chars int) time.Duration {
	var characterDelay, frameDelay int // us

	if sf.BaudRate <= 0 || sf.BaudRate > 19200 {
		characterDelay = 750
		frameDelay = 1750
	} else {
		characterDelay = 15000000 / sf.BaudRate
		frameDelay = 35000000 / sf.BaudRate
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

// verify confirms valid data(including slaveID,funcCode,response data)
func verify(reqSlaveID, rspSlaveID uint8, reqPDU, rspPDU ProtocolDataUnit) error {
	switch {
	case reqSlaveID != rspSlaveID: // Check slaveID same
		return fmt.Errorf("modbus: response slave id '%v' does not match request '%v'", rspSlaveID, reqSlaveID)

	case rspPDU.FuncCode != reqPDU.FuncCode: // Check correct function code returned (exception)
		return responseError(rspPDU)

	case rspPDU.Data == nil || len(rspPDU.Data) == 0: // check Empty response
		return fmt.Errorf("modbus: response data is empty")
	}
	return nil
}
