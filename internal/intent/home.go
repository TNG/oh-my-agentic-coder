package intent

import "os"

func osUserHome() (string, error) { return os.UserHomeDir() }
