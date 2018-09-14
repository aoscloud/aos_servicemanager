package launcher

import (
	"errors"
	"fmt"
	"hash/fnv"
	"os/exec"
	"os/user"
	"strconv"

	log "github.com/sirupsen/logrus"
)

/*******************************************************************************
 * Linux related API
 ******************************************************************************/

func (launcher *Launcher) createUser(id string) (userName string, err error) {
	launcher.mutex.Lock()
	defer launcher.mutex.Unlock()

	// convert id to hashed u32 value
	hash := fnv.New32a()
	hash.Write([]byte(id))

	// create user
	userName = "user_" + strconv.FormatUint(uint64(hash.Sum32()), 16)
	// if user exists
	if _, err = user.Lookup(userName); err == nil {
		return userName, errors.New("User already exists")
	}

	log.WithField("user", userName).Debug("Create user")

	if err = exec.Command("useradd", "-M", userName).Run(); err != nil {
		return userName, fmt.Errorf("Error creating user: %s", err)
	}

	return userName, nil
}
