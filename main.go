package main

import (
	"context"
	"fmt"
	"os"

	"github.com/bramvdbogaerde/go-scp/auth"
	"golang.org/x/crypto/ssh"
	"main/scp"
)

func main() {
	// Use SSH key authentication from the auth package
	// we ignore the host key in this example, please change this if you use this library
	clientConfig, _ := auth.PasswordKey(
		"claud",
		"tiger",
		ssh.InsecureIgnoreHostKey(),
	)

	// For other authentication methods see ssh.ClientConfig and ssh.AuthMethod

	// Create a new SCP client
	client := scp.NewClient("localhost:2022", &clientConfig)

	// Connect to the remote server
	err := client.Connect()
	if err != nil {
		fmt.Println("Couldn't establish a connection to the remote server ", err)
	}

	// Open a file
	f, _ := os.OpenFile("./hello.txt", os.O_RDWR|os.O_CREATE, 0777)

	// Close client connection after the file has been copied
	defer client.Close()

	// Close the file after it has been copied
	defer f.Close()

	// Finally, copy the file over
	// Usage: CopyFromFile(context, file, remotePath, permission)

	// the context can be adjusted to provide time-outs or inherit from other contexts if this is embedded in a larger application.
	err = client.CopyFromRemotePreserveProgressPassThru(
		context.Background(),
		f,
		"hello.txt",
		nil,
	)

	if err != nil {
		fmt.Println("Error while copying file ", err)
	}
}
