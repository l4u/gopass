package action

import (
	"context"
	"fmt"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/gopasspw/gopass/internal/action/exit"
	"github.com/gopasspw/gopass/internal/config"
	"github.com/gopasspw/gopass/internal/out"
	"github.com/gopasspw/gopass/internal/tree"
	"github.com/gopasspw/gopass/pkg/clipboard"
	"github.com/gopasspw/gopass/pkg/ctxutil"
	"github.com/gopasspw/gopass/pkg/debug"
	"github.com/gopasspw/gopass/pkg/gopass"
	"github.com/gopasspw/gopass/pkg/gopass/secrets"
	"github.com/gopasspw/gopass/pkg/pwgen"
	"github.com/gopasspw/gopass/pkg/pwgen/pwrules"
	"github.com/gopasspw/gopass/pkg/pwgen/xkcdgen"
	"github.com/gopasspw/gopass/pkg/termio"
	"github.com/urfave/cli/v2"
)

const (
	defaultLength     = 24
	defaultXKCDLength = 4
)

// defaultLengthFromEnv will determine the password length from the env variable
// GOPASS_PW_DEFAULT_LENGTH or fallback to the hard-coded default length.
// If the env variable is set by the user and is valid, the boolean return value
// will be true, otherwise it will be false.
func defaultLengthFromEnv(ctx context.Context) (int, bool) {
	def := defaultLength

	if l := config.Int(ctx, "generate.length"); l > 0 {
		def = l
	}

	lengthStr, isSet := os.LookupEnv("GOPASS_PW_DEFAULT_LENGTH")
	if !isSet {
		return def, false
	}
	length, err := strconv.Atoi(lengthStr)
	if err != nil {
		return def, false
	}
	if length < 1 {
		return def, false
	}

	return length, true
}

var reNumber = regexp.MustCompile(`^\d+$`)

// Generate and save a password.
func (s *Action) Generate(c *cli.Context) error {
	ctx := ctxutil.WithGlobalFlags(c)
	ctx = WithClip(ctx, c.Bool("clip"))
	force := c.Bool("force")
	edit := c.Bool("edit") // nolint:ifshort

	args, kvps := parseArgs(c)
	name := args.Get(0)
	key, length := keyAndLength(args)

	ctx = ctxutil.WithForce(ctx, force)

	// ask for name of the secret if it wasn't provided already.
	if name == "" {
		var err error
		name, err = termio.AskForString(ctx, "Which name do you want to use?", "")
		if err != nil || name == "" {
			return exit.Error(exit.NoName, err, "please provide a password name")
		}
	}

	// ask for confirmation before overwriting existing entry.
	if !force { // don't check if it's force anyway.
		if s.Store.Exists(ctx, name) && key == "" && !termio.AskForConfirmation(ctx, fmt.Sprintf("An entry already exists for %s. Overwrite the current password?", name)) {
			return exit.Error(exit.Aborted, nil, "user aborted. not overwriting your current password")
		}
	}

	// generate password.
	password, err := s.generatePassword(ctx, c, length, name)
	if err != nil {
		return err
	}

	// display or copy to clipboard.
	if err := s.generateCopyOrPrint(ctx, c, name, key, password); err != nil {
		return err
	}

	// write generated password to store.
	ctx, err = s.generateSetPassword(ctx, name, key, password, kvps)
	if err != nil {
		return err
	}

	// if requested launch editor to add more data to the generated secret.
	if edit && termio.AskForConfirmation(ctx, fmt.Sprintf("Do you want to add more data for %s?", name)) {
		c.Context = ctx
		if err := s.Edit(c); err != nil {
			return exit.Error(exit.Unknown, err, "failed to edit %q: %s", name, err)
		}
	}

	return nil
}

func keyAndLength(args argList) (string, string) {
	key := args.Get(1)
	length := args.Get(2)

	// generate can be called with one positional arg or two.
	// * one - the desired length for the "master" secret itself
	// * two - the key in a YAML doc and the length for a secret generated for this
	//   key only.
	if length == "" && key != "" && reNumber.MatchString(key) {
		length = key
		key = ""
	}

	return key, length
}

// generateCopyOrPrint will print the password to the screen or copy to the
// clipboard.
func (s *Action) generateCopyOrPrint(ctx context.Context, c *cli.Context, name, key, password string) error {
	entry := name
	if key != "" {
		entry += " " + key
	}

	out.OKf(ctx, "Password for entry %q generated", entry)

	// copy to clipboard if:
	// - explicitly requested with -c
	// - autoclip=true, but only if output is not being redirected.
	if IsClip(ctx) || (s.cfg.GetBool("core.autoclip") && ctxutil.IsTerminal(ctx)) {
		if err := clipboard.CopyTo(ctx, name, []byte(password), s.cfg.GetInt("core.cliptimeout")); err != nil {
			return exit.Error(exit.IO, err, "failed to copy to clipboard: %s", err)
		}
		// if autoclip is on and we're not printing the password to the terminal
		// at least leave a notice that we did indeed copy it.
		if s.cfg.GetBool("core.autoclip") && !c.Bool("print") {
			out.Print(ctx, "Copied to clipboard")

			return nil
		}
	}

	if !c.Bool("print") {
		out.Printf(ctx, "Not printing secrets by default. Use 'gopass show %s' to display the password.", entry)

		return nil
	}

	if c.IsSet("print") && !c.Bool("print") && config.Bool(ctx, "core.showsafecontent") {
		debug.Log("safecontent suppressing printing")

		return nil
	}

	out.Printf(
		ctx,
		"⚠ The generated password is:\n\n%s\n",
		out.Secret(password),
	)

	return nil
}

func hasPwRuleForSecret(ctx context.Context, name string) (string, pwrules.Rule) {
	for name != "" && name != "." {
		d := path.Base(name)
		if r, found := pwrules.LookupRule(ctx, d); found {
			return d, r
		}
		name = path.Dir(name)
	}

	return "", pwrules.Rule{}
}

// generatePassword will run through the password generation steps.
func (s *Action) generatePassword(ctx context.Context, c *cli.Context, length, name string) (string, error) {
	if domain, rule := hasPwRuleForSecret(ctx, name); domain != "" && !c.Bool("force") {
		return s.generatePasswordForRule(ctx, c, length, name, domain, rule)
	}

	symbols := false
	if c.IsSet("symbols") {
		symbols = c.Bool("symbols")
	} else {
		cfg := config.FromContext(ctx)
		if cfg.IsSet("generate.symbols") {
			symbols = cfg.GetBool("generate.symbols")
		}
	}

	var pwlen int
	if length == "" {
		pwlength, err := getPwLengthFromEnvOrAskUser(ctx)
		if err != nil {
			return "", err
		}
		pwlen = pwlength
	} else {
		iv, err := strconv.Atoi(length)
		if err != nil {
			return "", exit.Error(exit.Usage, err, "password length must be a number")
		}
		pwlen = iv
	}

	if pwlen < 1 {
		return "", exit.Error(exit.Usage, nil, "password length must not be zero")
	}

	switch c.String("generator") {
	case "xkcd":
		return s.generatePasswordXKCD(ctx, c, length)
	case "memorable":
		if c.Bool("strict") {
			return pwgen.GenerateMemorablePassword(pwlen, symbols, true), nil
		}

		return pwgen.GenerateMemorablePassword(pwlen, symbols, false), nil
	case "external":
		return pwgen.GenerateExternal(pwlen)
	default:
		if c.Bool("strict") {
			return pwgen.GeneratePasswordWithAllClasses(pwlen, symbols)
		}

		return pwgen.GeneratePassword(pwlen, symbols), nil
	}
}

// getPwLengthFromEnvOrAskUser either determines the password length through an
// environment variable or asks the user to set one.
// This function assumes that if the length is set via the environment variable,
// the user has already made a conscious decision and does not need to be asked
// again.
func getPwLengthFromEnvOrAskUser(ctx context.Context) (int, error) {
	var pwlen int
	candidateLength, isCustom := defaultLengthFromEnv(ctx)
	if !isCustom {
		question := "How long should the password be?"
		iv, err := termio.AskForInt(ctx, question, candidateLength)
		if err != nil {
			return 0, exit.Error(exit.Usage, err, "password length must be a number")
		}
		pwlen = iv
	} else {
		pwlen = candidateLength
	}

	return pwlen, nil
}

func clamp(min, max, value int) int {
	if value < min {
		return min
	}

	if value > max {
		return max
	}

	return value
}

func (s *Action) generatePasswordForRule(ctx context.Context, c *cli.Context, length, name, domain string, rule pwrules.Rule) (string, error) {
	out.Noticef(ctx, "Using password rules for %s ...", domain)
	wl := 16
	if iv, err := strconv.Atoi(length); err == nil {
		wl = clamp(rule.Minlen, rule.Maxlen, iv)
	}

	question := fmt.Sprintf("How long should the password be? (min: %d, max: %d)", rule.Minlen, rule.Maxlen)
	iv, err := termio.AskForInt(ctx, question, wl)
	if err != nil {
		return "", exit.Error(exit.Usage, err, "password length must be a number")
	}

	iv = clamp(rule.Minlen, rule.Maxlen, iv)

	pw := pwgen.NewCrypticForDomain(ctx, iv, domain).Password()
	if pw == "" {
		return "", fmt.Errorf("failed to generate password for %s", domain)
	}

	return pw, nil
}

// generatePasswordXKCD walks through the steps necessary to create an XKCD-style
// password.
func (s *Action) generatePasswordXKCD(ctx context.Context, c *cli.Context, length string) (string, error) {
	xkcdSeparator := " "
	if c.IsSet("sep") {
		xkcdSeparator = c.String("sep")
	}

	var pwlen int
	if length == "" {
		candidateLength := defaultXKCDLength
		question := "How many words should be combined to a password?"
		iv, err := termio.AskForInt(ctx, question, candidateLength)
		if err != nil {
			return "", exit.Error(exit.Usage, err, "password length must be a number")
		}
		pwlen = iv
	} else {
		iv, err := strconv.Atoi(length)
		if err != nil {
			return "", exit.Error(exit.Usage, err, "password length must be a number: %s", err)
		}
		pwlen = iv
	}

	if pwlen < 1 {
		return "", exit.Error(exit.Usage, nil, "password length must not be zero")
	}

	return xkcdgen.RandomLengthDelim(pwlen, xkcdSeparator, c.String("lang"))
}

// generateSetPassword will update or create a secret.
func (s *Action) generateSetPassword(ctx context.Context, name, key, password string, kvps map[string]string) (context.Context, error) {
	// set a single key in an entry.
	if key != "" {
		sec, err := s.Store.Get(ctx, name)
		if err != nil {
			return ctx, exit.Error(exit.Encrypt, err, "failed to set key %q of %q: %s", key, name, err)
		}

		setMetadata(sec, kvps)
		_ = sec.Set(key, password)
		if err := s.Store.Set(ctxutil.WithCommitMessage(ctx, "Generated password for key"), name, sec); err != nil {
			return ctx, exit.Error(exit.Encrypt, err, "failed to set key %q of %q: %s", key, name, err)
		}

		return ctx, nil
	}

	// replace password in existing secret.
	if s.Store.Exists(ctx, name) {
		ctx, err := s.generateReplaceExisting(ctx, name, key, password, kvps)
		if err == nil {
			return ctx, nil
		}

		out.Errorf(ctx, "Failed to read existing secret. Creating anew. Error: %s", err.Error())
	}

	// generate a completely new secret.
	var sec gopass.Secret
	sec = secrets.New()
	sec.SetPassword(password)
	if u := hasChangeURL(ctx, name); u != "" {
		_ = sec.Set("password-change-url", u)
	}

	if content, found := s.renderTemplate(ctx, name, []byte(password)); found {
		nSec := secrets.NewAKV()
		if _, err := nSec.Write(content); err == nil {
			sec = nSec
		} else {
			debug.Log("failed to handle template: %s", err)
		}
	}

	if err := s.Store.Set(ctxutil.WithCommitMessage(ctx, "Generated Password"), name, sec); err != nil {
		return ctx, exit.Error(exit.Encrypt, err, "failed to create %q: %s", name, err)
	}

	return ctx, nil
}

func hasChangeURL(ctx context.Context, name string) string {
	p := strings.Split(name, "/")
	for i := len(p) - 1; i > 0; i-- {
		if u := pwrules.LookupChangeURL(ctx, p[i]); u != "" {
			return u
		}
	}

	return ""
}

func (s *Action) generateReplaceExisting(ctx context.Context, name, key, password string, kvps map[string]string) (context.Context, error) {
	sec, err := s.Store.Get(ctx, name)
	if err != nil {
		return ctx, exit.Error(exit.Encrypt, err, "failed to set key %q of %q: %s", key, name, err)
	}

	setMetadata(sec, kvps)
	sec.SetPassword(password)
	if err := s.Store.Set(ctxutil.WithCommitMessage(ctx, "Generated password for YAML key"), name, sec); err != nil {
		return ctx, exit.Error(exit.Encrypt, err, "failed to set key %q of %q: %s", key, name, err)
	}

	return ctx, nil
}

func setMetadata(sec gopass.Secret, kvps map[string]string) {
	for k, v := range kvps {
		_ = sec.Set(k, v)
	}
}

// CompleteGenerate implements the completion heuristic for the generate command.
func (s *Action) CompleteGenerate(c *cli.Context) {
	ctx := ctxutil.WithGlobalFlags(c)
	if c.Args().Len() < 1 {
		return
	}
	needle := c.Args().Get(0) // nolint:ifshort

	_, err := s.Store.IsInitialized(ctx) // important to make sure the structs are not nil.
	if err != nil {
		out.Errorf(ctx, "Store not initialized: %s", err)

		return
	}

	list, err := s.Store.List(ctx, tree.INF)
	if err != nil {
		return
	}

	if strings.Contains(needle, "/") {
		list = filterPrefix(uniq(extractEmails(list)), path.Base(needle))
	} else {
		list = filterPrefix(uniq(extractDomains(list)), needle)
	}

	for _, v := range list {
		fmt.Fprintln(stdout, bashEscape(v))
	}
}

func extractEmails(list []string) []string {
	results := make([]string, 0, len(list))
	for _, e := range list {
		e = path.Base(e)
		if strings.Contains(e, "@") || strings.Contains(e, "_") {
			results = append(results, e)
		}
	}

	return results
}

var reDomain = regexp.MustCompile(`^(?i)([a-z0-9]+(-[a-z0-9]+)*\.)+[a-z]{2,}$`)

func extractDomains(list []string) []string {
	results := make([]string, 0, len(list))
	for _, e := range list {
		e = path.Base(e)
		if reDomain.MatchString(e) {
			results = append(results, e)
		}
	}

	return results
}

func uniq(in []string) []string {
	set := make(map[string]struct{}, len(in))
	for _, e := range in {
		set[e] = struct{}{}
	}

	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
}

func filterPrefix(in []string, prefix string) []string {
	out := make([]string, 0, len(in))
	for _, e := range in {
		if strings.HasPrefix(e, prefix) {
			out = append(out, e)
		}
	}

	return out
}
