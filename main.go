package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/chickenzord/mailgrep"
	"github.com/chickenzord/mailgrep/filter"
	"github.com/emersion/go-imap"
	"github.com/joho/godotenv"
	"gopkg.in/yaml.v2"
)

type Profile struct {
	Name          string        `yaml:"name"`
	VpnConfig     string        `yaml:"vpn_config"`
	OtpPrompt     string        `yaml:"otp_prompt"`
	SearchDelay   time.Duration `yaml:"search_delay"`
	SearchSender  string        `yaml:"search_sender"`
	SearchMailbox string        `yaml:"search_mailbox"`
	SearchWithin  time.Duration `yaml:"search_within"`
	SearchRegex   string        `yaml:"search_regex"`
	Imap          ImapConfig    `yaml:"imap"`
}

type ImapConfig struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

type Config struct {
	Profiles []Profile `yaml:"profiles"`
}

func (c *Config) GetProfile(name string) (*Profile, error) {
	for _, profile := range c.Profiles {
		if profile.Name == name {
			return &profile, nil
		}
	}

	return nil, fmt.Errorf("Profile not found: " + name)
}

func main() {
	godotenv.Overload()
	var configFile string
	if val, ok := os.LookupEnv("EMPATPULUH_CONFIG"); ok {
		configFile = val
	} else {
		configFile = filepath.Join(os.Getenv("HOME"), ".empatpuluh.yml")
	}

	if len(os.Args) == 1 {
		fmt.Printf("usage: %s [list|connect]\n", os.Args[0])
		os.Exit(0)
	}

	// Open and decode config
	var config Config
	file, err := os.Open(configFile)
	if err != nil {
		panic(err)
	}
	defer file.Close()
	err = yaml.NewDecoder(file).Decode(&config)
	if err != nil {
		panic(err)
	}

	runInBackground := false
	if len(os.Args) > 3 && os.Args[3] == "--background" {
		runInBackground = true
	}

	switch os.Args[1] {
	case "list":
		for _, profile := range config.Profiles {
			fmt.Println(profile.Name)
		}
	case "connect":
		profileName := os.Args[2]
		fmt.Printf("Connect using profile: %s\n", profileName)
		profile, err := config.GetProfile(profileName)
		if err != nil {
			panic(err)
		}

		connect(profile, runInBackground)
	}
}

func connect(profile *Profile, runInBackground bool) {
	// Prepare command
	cmd := exec.Command("openfortivpn", "-c", profile.VpnConfig)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Fatal(err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		log.Fatal(err)
	}
	cmd.Stderr = os.Stderr

	// Start command
	if err := cmd.Start(); err != nil {
		log.Fatal(err)
	}
	if runInBackground {
		// Detach the process and run in the background
		connectInBackground(cmd)
	} else {
		defer cmd.Wait()
	}

	// Wait for OTP prompt
	checkPrompt := func(bytes []byte) bool {
		frags := strings.Split(string(bytes), "\n")
		if len(frags) == 0 {
			return false
		}

		last := frags[len(frags)-1]

		return strings.HasPrefix(last, profile.OtpPrompt)
	}
	prompt := make(chan bool, 1)
	go func(ch chan<- bool) {
		scanner := bufio.NewScanner(stdout)
		scanner.Split(bufio.ScanBytes)

		buff := []byte{}
		for scanner.Scan() {
			bytes := scanner.Bytes()
			fmt.Print(string(bytes))
			buff = append(buff, bytes...)
			if checkPrompt(buff) {
				ch <- true
			}
		}
	}(prompt)
	<-prompt

	fmt.Println("Getting OTP")
	fmt.Printf("Delaying %v before search\n", profile.SearchDelay)
	time.Sleep(profile.SearchDelay)

	chOTP := make(chan string, 1)
	searchInterval := time.Second
	searchTimeout := 50 * time.Second
	go func(profile *Profile) {
		for {
			otp := searchOTP(profile)
			if otp != "" {
				chOTP <- otp
				break
			}
			fmt.Printf("OTP not found, sleep %s\n", searchInterval)
			time.Sleep(searchInterval)
		}
	}(profile)

	select {
	case otp := <-chOTP:
		fmt.Printf("Found the OTP: %s\n", otp)
		io.WriteString(stdin, otp)
		io.WriteString(stdin, "\n")
	case <-time.After(searchTimeout):
		cmd.Process.Kill()
		fmt.Printf("Timeout %s reached\n", searchTimeout)
	}
}

func connectInBackground(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	// Detach the process from the terminal
	_ = syscall.Dup2(int(os.Stdin.Fd()), int(os.Stdout.Fd()))
	_ = syscall.Dup2(int(os.Stdin.Fd()), int(os.Stderr.Fd()))
	syscall.Setsid()
}

func searchOTP(p *Profile) string {
	messages, err := mailgrep.ListEmail(
		&mailgrep.ImapConfig{
			Address:  fmt.Sprintf("%s:%d", p.Imap.Host, p.Imap.Port),
			Username: p.Imap.Username,
			Password: p.Imap.Password,
		},
		&mailgrep.ListRequest{
			Mailbox: p.SearchMailbox,
			Filters: []filter.Filter{
				filter.SenderAddress(p.SearchSender),
				filter.Within(p.SearchWithin),
			},
		},
	)
	if err != nil {
		panic(err)
	}

	otpFromSubject := func(msg imap.Message, regex string) string {
		re := regexp.MustCompile(regex)
		match := re.FindStringSubmatch(msg.Envelope.Subject)
		if len(match) > 1 {
			return match[1]
		}

		return ""
	}

	if len(messages) > 0 {
		return otpFromSubject(messages[0], p.SearchRegex)
	}

	return ""
}
