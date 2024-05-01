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
		"pass",
		ssh.InsecureIgnoreHostKey(),
	)

	client := scp.NewConfigurer("localhost:22", &clientConfig).Create()

	// Connect to the remote server
	err := client.Connect()
	if err != nil {
		fmt.Println("Couldn't establish a connection to the remote server ", err)
	}

	// Open a file
	f, _ := os.OpenFile("./hello.txt", os.O_CREATE, 0644)

	// Close client connection after the file has been copied
	defer client.Close()

	// Close the file after it has been copied
	defer f.Close()

	fileInfos, err := client.CopyFromRemoteFileInfos(context.Background(), f, "hello.txt", nil)

	if err != nil {
		fmt.Println("Error while copying file ", err)
		return
	}

	fmt.Println(fileInfos)

	fileStat, err := os.Stat("./hello.txt")
	if err != nil {
		fmt.Println("Error while getting file stat ", err)
	}

	if err != nil {
		fmt.Println("Error while getting file stat ", err)
	}

	if fileStat.Size() != fileInfos.Size {
		fmt.Println("File size does not match")
	}

	fileStat, _ = os.Stat("C:Users/claud/hello.txt")
	fmt.Println(fileStat.Mode().Perm())
	fmt.Println(os.FileMode(fileInfos.Permissions))
	if fileStat.Mode().Perm() != os.FileMode(fileInfos.Permissions) {
		fmt.Println("File permissions do not match")
	}

	if fileStat.ModTime().Unix() != fileInfos.Mtime {
		fmt.Println(fileStat.ModTime().Local().Unix())
		fmt.Println(fileInfos.Mtime)
		fmt.Println("File modification time does not match")
	}

}
