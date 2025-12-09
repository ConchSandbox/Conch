package main

import (
	"fmt"
	"os"

	"github.com/openeuler/conch/pkg/common"
)

func main() {
	fmt.Println("conchd - Conch agentd")
	fmt.Println("========================================")

	message := common.GetMessage("conchd")
	fmt.Println(message)

	if len(os.Args) > 1 {
		fmt.Printf("arguments: %v\n", os.Args[1:])
	}
}
