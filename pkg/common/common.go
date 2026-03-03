package common

import "fmt"

func GetMessage(from string) string {
	return fmt.Sprintf("This is from: %s", from)
}
