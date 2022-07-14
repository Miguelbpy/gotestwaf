package scanner

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/gorilla/websocket"
	"github.com/pkg/errors"
	"github.com/schollz/progressbar/v3"
	"google.golang.org/grpc/metadata"

	"github.com/wallarm/gotestwaf/internal/config"
	"github.com/wallarm/gotestwaf/internal/db"
	"github.com/wallarm/gotestwaf/internal/openapi"
	"github.com/wallarm/gotestwaf/internal/payload/encoder"
)

const (
	preCheckVector        = "<script>alert('union select password from users')</script>"
	wsPreCheckReadTimeout = time.Second * 1
)

type testWork struct {
	setName         string
	caseName        string
	payload         string
	encoder         string
	placeholder     string
	testType        string
	isTruePositive  bool
	testHeaderValue string
}

// Scanner allows you to test WAF in various ways with given payloads.
type Scanner struct {
	logger *log.Logger

	cfg *config.Config
	db  *db.DB

	httpClient *HTTPClient
	grpcConn   *GRPCConn
	wsClient   *websocket.Dialer

	requestTemplates openapi.Templates
	router           routers.Router

	isTestEnv bool
}

// New creates a new Scanner.
func New(
	logger *log.Logger,
	cfg *config.Config,
	db *db.DB,
	requestTemplates openapi.Templates,
	router routers.Router,
	isTestEnv bool,
) (*Scanner, error) {
	httpClient, err := NewHTTPClient(cfg)
	if err != nil {
		return nil, errors.Wrap(err, "couldn't create HTTP client")
	}

	grpcConn, err := NewGRPCConn(cfg)
	if err != nil {
		return nil, errors.Wrap(err, "couldn't create gRPC client")
	}

	return &Scanner{
		logger:           logger,
		cfg:              cfg,
		db:               db,
		httpClient:       httpClient,
		grpcConn:         grpcConn,
		requestTemplates: requestTemplates,
		router:           router,
		wsClient:         websocket.DefaultDialer,
		isTestEnv:        isTestEnv,
	}, nil
}

// CheckGRPCAvailability checks if the gRPC server is available at the given URL.
func (s *Scanner) CheckGRPCAvailability() {
	s.logger.Printf("gRPC pre-check: in progress")
	available, err := s.grpcConn.CheckAvailability()
	if err != nil {
		s.logger.Printf("gRPC pre-check: connection is not available, reason: %s\n", err)
	}
	if available {
		s.logger.Printf("gRPC pre-check: gRPC is available")
	} else {
		s.logger.Printf("gRPC pre-check: gRPC is not available")
	}
}

// WAFBlockCheck checks if WAF exists and blocks malicious requests.
func (s *Scanner) WAFBlockCheck() error {
	s.logger.Println("WAF pre-check. URL to check:", s.cfg.URL)

	if !s.cfg.SkipWAFBlockCheck {
		ok, httpStatus, err := s.preCheck(preCheckVector)
		if err != nil {
			if s.cfg.BlockConnReset && (errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET)) {
				s.logger.Println("Connection reset, trying benign request to make sure that service is available")
				blockedBenign, httpStatusBenign, errBenign := s.preCheck("")
				if !blockedBenign {
					s.logger.Printf("Service is available (HTTP status: %d), WAF resets connections. Consider this behavior as block", httpStatusBenign)
					ok = true
				}
				if errBenign != nil {
					return errors.Wrap(errBenign, "running benign request pre-check")
				}
			} else {
				return errors.Wrap(err, "running pre-check")
			}
		}

		if !ok {
			return errors.Errorf("WAF was not detected. "+
				"Please use the '--blockStatusCode' or '--blockRegex' flags. Use '--help' for additional info."+
				"\nBaseline attack status code: %v\n", httpStatus)
		}

		s.logger.Printf("WAF pre-check: OK. Blocking status code: %v\n", httpStatus)
	} else {
		s.logger.Println("WAF pre-check: SKIPPED")
	}

	return nil
}

// preCheck sends given payload during the pre-check stage.
func (s *Scanner) preCheck(payload string) (blocked bool, statusCode int, err error) {
	body, code, err := s.httpClient.SendPayload(context.Background(), s.cfg.URL, "URLParam", "URL", payload, "")
	if err != nil {
		return false, 0, err
	}
	blocked, err = s.checkBlocking(body, code)
	if err != nil {
		return false, 0, err
	}
	return blocked, code, nil
}

// WAFwsBlockCheck checks if WebSocket exists and is protected by WAF.
func (s *Scanner) WAFwsBlockCheck() {
	s.logger.Println("WebSocket pre-check. URL to check:", s.cfg.WebSocketURL)

	if !s.cfg.SkipWAFBlockCheck {
		available, blocked, err := s.wsPreCheck()
		if !available && err != nil {
			s.logger.Printf("WebSocket pre-check: connection is not available, reason: %s\n", err)
		}
		if available && blocked {
			s.logger.Printf("WebSocket is available and payloads are blocked by the WAF, reason: %s\n", err)
		}
		if available && !blocked {
			s.logger.Println("WebSocket is available and payloads are not blocked by the WAF")
		}
	} else {
		s.logger.Println("WebSocket pre-check: SKIPPED")
	}
}

// wsPreCheck sends the payload and analyzes response.
func (s *Scanner) wsPreCheck() (available, blocked bool, err error) {
	wsClient, _, err := s.wsClient.Dial(s.cfg.WebSocketURL, nil)
	if err != nil {
		return false, false, err
	}
	defer wsClient.Close()

	wsPreCheckVectors := [...]string{
		fmt.Sprintf("{\"message\": \"%[1]s\", \"%[1]s\": \"%[1]s\"}", preCheckVector),
		preCheckVector,
	}

	block := make(chan error)
	receivedCtr := 0

	go func() {
		defer close(block)
		for {
			wsClient.SetReadDeadline(time.Now().Add(wsPreCheckReadTimeout))
			_, _, err := wsClient.ReadMessage()
			if err != nil {
				return
			}
			receivedCtr++
		}
	}()

	for i, payload := range wsPreCheckVectors {
		err = wsClient.WriteMessage(websocket.TextMessage, []byte(payload))
		if err != nil && i == 0 {
			return true, false, err
		} else if err != nil {
			return true, true, nil
		}
	}

	if _, open := <-block; !open && receivedCtr != len(wsPreCheckVectors) {
		return true, true, nil
	}

	return true, false, nil
}

// Run starts a host scan to check WAF security.
func (s *Scanner) Run(ctx context.Context) error {
	gn := s.cfg.Workers
	var wg sync.WaitGroup
	wg.Add(gn)

	defer s.grpcConn.Close()

	rand.Seed(time.Now().UnixNano())

	s.logger.Printf("Scanning %s\n", s.cfg.URL)
	s.logger.Println("Scanning started")
	defer s.logger.Println("Scanning finished")

	start := time.Now()
	defer s.logger.Println("Scanning Time: ", time.Since(start))

	testChan := s.produceTests(ctx, gn)

	bar := progressbar.NewOptions64(
		int64(s.db.GetNumberOfAllTestCases()),
		progressbar.OptionShowCount(),
		progressbar.OptionSetPredictTime(false),
		progressbar.OptionFullWidth(),
		progressbar.OptionClearOnFinish(),
		progressbar.OptionSetDescription("Sending requests..."),
		progressbar.OptionSetTheme(progressbar.Theme{
			Saucer:        "=",
			SaucerHead:    ">",
			SaucerPadding: " ",
			BarStart:      "[",
			BarEnd:        "]",
		}),
	)

	for e := 0; e < gn; e++ {
		go func(ctx context.Context) {
			defer wg.Done()
			for {
				select {
				case w, ok := <-testChan:
					if !ok {
						return
					}
					time.Sleep(time.Duration(s.cfg.SendDelay+rand.Intn(s.cfg.RandomDelay)) * time.Millisecond)

					if err := s.scanURL(ctx, w); err != nil {
						s.logger.Println(err)
					}
					bar.Add(1)
				case <-ctx.Done():
					return
				}
			}
		}(ctx)
	}

	wg.Wait()
	if errors.Is(ctx.Err(), context.Canceled) {
		return ctx.Err()
	}

	return nil
}

// checkBlocking checks the response status-code or request body using
// a regular expression to determine if the request has been blocked.
func (s *Scanner) checkBlocking(body string, statusCode int) (bool, error) {
	if s.cfg.BlockRegex != "" {
		m, _ := regexp.MatchString(s.cfg.BlockRegex, body)
		return m, nil
	}
	return statusCode == s.cfg.BlockStatusCode, nil
}

// checkPass checks the response status-code or request body using
// a regular expression to determine if the request has been passed.
func (s *Scanner) checkPass(body string, statusCode int) (bool, error) {
	if s.cfg.PassRegex != "" {
		m, _ := regexp.MatchString(s.cfg.PassRegex, body)
		return m, nil
	}

	for _, code := range s.cfg.PassStatusCode {
		if statusCode == code {
			return true, nil
		}
	}

	return false, nil
}

// produceTests generates all combinations of payload, encoder, and placeholder
// for n goroutines.
func (s *Scanner) produceTests(ctx context.Context, n int) <-chan *testWork {
	testChan := make(chan *testWork, n)
	testCases := s.db.GetTestCases()

	go func() {
		defer close(testChan)

		var testHeaderValue string

		for _, t := range testCases {
			for _, payload := range t.Payloads {
				for _, e := range t.Encoders {
					for _, placeholder := range t.Placeholders {
						if s.isTestEnv {
							testHeaderValue = fmt.Sprintf(
								"set=%s,name=%s,placeholder=%s,encoder=%s",
								t.Set, t.Name, placeholder, e,
							)
						} else {
							testHeaderValue = ""
						}
						wrk := &testWork{t.Set,
							t.Name,
							payload,
							e,
							placeholder,
							t.Type,
							t.IsTruePositive,
							testHeaderValue,
						}
						select {
						case testChan <- wrk:
						case <-ctx.Done():
							return
						}
					}
				}
			}
		}
	}()
	return testChan
}

// scanURL scans the host with the given combination of payload, encoder and
// placeholder.
func (s *Scanner) scanURL(ctx context.Context, w *testWork) error {
	var (
		respHeaders http.Header
		body        string
		statusCode  int
		err         error
	)

	if w.encoder == encoder.DefaultGRPCEncoder.GetName() {
		if !s.grpcConn.IsAvailable() {
			return nil
		}

		newCtx := ctx
		if w.testHeaderValue != "" {
			newCtx = metadata.AppendToOutgoingContext(ctx, "X-GoTestWAF-Test", w.testHeaderValue)
		}

		body, statusCode, err = s.grpcConn.Send(newCtx, w.encoder, w.payload)

		_, _, _, _, err = s.updateDB(ctx, w, nil, nil, nil, nil, nil,
			statusCode, nil, body, err, "", true)

		return err
	}

	if s.requestTemplates == nil {
		body, statusCode, err = s.httpClient.SendPayload(ctx, s.cfg.URL, w.placeholder, w.encoder, w.payload, w.testHeaderValue)

		_, _, _, _, err = s.updateDB(ctx, w, nil, nil, nil, nil, nil,
			statusCode, nil, body, err, "", false)

		return err
	}

	templates := s.requestTemplates[w.placeholder]

	encodedPayload, err := encoder.Apply(w.encoder, w.payload)
	if err != nil {
		return errors.Wrap(err, "encoding payload")
	}

	var passedTest *db.Info
	var blockedTest *db.Info
	var unresolvedTest *db.Info
	var failedTest *db.Info
	var additionalInfo string

	for _, template := range templates {
		req, err := template.CreateRequest(ctx, w.placeholder, encodedPayload)
		if err != nil {
			return errors.Wrap(err, "create request from template")
		}

		respHeaders, body, statusCode, err = s.httpClient.SendRequest(req, w.testHeaderValue)

		additionalInfo = fmt.Sprintf("%s %s", template.Method, template.Path)

		passedTest, blockedTest, unresolvedTest, failedTest, err =
			s.updateDB(ctx, w, passedTest, blockedTest, unresolvedTest, failedTest,
				req, statusCode, respHeaders, body, err, additionalInfo, false)

		s.db.AddToScannedPaths(template.Method, template.Path)

		if err != nil {
			return err
		}
	}

	return nil
}

// updateDB updates the success of a query in the database.
func (s *Scanner) updateDB(
	ctx context.Context,
	w *testWork,
	passedTest *db.Info,
	blockedTest *db.Info,
	unresolvedTest *db.Info,
	failedTest *db.Info,
	req *http.Request,
	respStatusCode int,
	respHeaders http.Header,
	respBody string,
	sendErr error,
	additionalInfo string,
	isGRPC bool,
) (
	updPassedTest *db.Info,
	updBlockedTest *db.Info,
	updUnresolvedTest *db.Info,
	updFailedTest *db.Info,
	err error,
) {
	updPassedTest = passedTest
	updBlockedTest = blockedTest
	updUnresolvedTest = unresolvedTest
	updFailedTest = failedTest

	info := w.toInfo(respStatusCode)

	var blockedByReset bool
	if sendErr != nil {
		if errors.Is(sendErr, io.EOF) || errors.Is(sendErr, syscall.ECONNRESET) {
			if s.cfg.BlockConnReset {
				blockedByReset = true
			} else {
				if updUnresolvedTest == nil {
					updUnresolvedTest = info
					s.db.UpdateNaTests(updUnresolvedTest, s.cfg.IgnoreUnresolved, s.cfg.NonBlockedAsPassed, w.isTruePositive)
				}
				if len(additionalInfo) != 0 {
					unresolvedTest.AdditionalInfo = append(unresolvedTest.AdditionalInfo, additionalInfo)
				}

				return
			}
		} else {
			if updFailedTest == nil {
				updFailedTest = info
				s.db.UpdateFailedTests(updFailedTest)
			}
			if len(additionalInfo) != 0 {
				updFailedTest.AdditionalInfo = append(updFailedTest.AdditionalInfo, sendErr.Error())
			}

			s.logger.Printf("http sending: %s\n", sendErr.Error())

			return
		}
	}

	var blocked, passed bool
	if blockedByReset {
		blocked = true
	} else {
		blocked, err = s.checkBlocking(respBody, respStatusCode)
		if err != nil {
			return nil, nil, nil, nil,
				errors.Wrap(err, "failed to check blocking")
		}

		passed, err = s.checkPass(respBody, respStatusCode)
		if err != nil {
			return nil, nil, nil, nil,
				errors.Wrap(err, "failed to check passed or not")
		}
	}

	if s.requestTemplates != nil && !isGRPC {
		route, pathParams, routeErr := s.router.FindRoute(req)
		if routeErr != nil {
			// split Method and url template
			additionalInfoParts := strings.Split(additionalInfo, " ")
			if len(additionalInfoParts) < 2 {
				return nil, nil, nil, nil,
					errors.Wrap(routeErr, "couldn't find request route")
			}

			req.URL.Path = additionalInfoParts[1]
			route, pathParams, routeErr = s.router.FindRoute(req)
			if routeErr != nil {
				return nil, nil, nil, nil,
					errors.Wrap(routeErr, "couldn't find request route")
			}
		}

		inputReuqestValidation := &openapi3filter.RequestValidationInput{
			Request:     req,
			PathParams:  pathParams,
			QueryParams: req.URL.Query(),
			Route:       route,
		}

		responseValidationInput := &openapi3filter.ResponseValidationInput{
			RequestValidationInput: inputReuqestValidation,
			Status:                 respStatusCode,
			Header:                 respHeaders,
			Body:                   ioutil.NopCloser(strings.NewReader(respBody)),
			Options: &openapi3filter.Options{
				IncludeResponseStatus: true,
			},
		}

		if validationErr := openapi3filter.ValidateResponse(ctx, responseValidationInput); validationErr == nil && !blocked {
			if updPassedTest == nil {
				updPassedTest = info
				s.db.UpdatePassedTests(updPassedTest)
			}
			if len(additionalInfo) != 0 {
				updPassedTest.AdditionalInfo = append(updPassedTest.AdditionalInfo, additionalInfo)
			}
		} else {
			if updBlockedTest == nil {
				updBlockedTest = info
				s.db.UpdateBlockedTests(updBlockedTest)
			}
			if len(additionalInfo) != 0 {
				updBlockedTest.AdditionalInfo = append(updBlockedTest.AdditionalInfo, additionalInfo)
			}
		}

		return
	}

	if (blocked && passed) || (!blocked && !passed) {
		if updUnresolvedTest == nil {
			updUnresolvedTest = info
			s.db.UpdateNaTests(updUnresolvedTest, s.cfg.IgnoreUnresolved, s.cfg.NonBlockedAsPassed, w.isTruePositive)
		}
		if len(additionalInfo) != 0 {
			unresolvedTest.AdditionalInfo = append(unresolvedTest.AdditionalInfo, additionalInfo)
		}
	} else {
		if blocked {
			if updBlockedTest == nil {
				updBlockedTest = info
				s.db.UpdateBlockedTests(updBlockedTest)
			}
			if len(additionalInfo) != 0 {
				updBlockedTest.AdditionalInfo = append(updBlockedTest.AdditionalInfo, additionalInfo)
			}
		} else {
			if updPassedTest == nil {
				updPassedTest = info
				s.db.UpdatePassedTests(updPassedTest)
			}
			if len(additionalInfo) != 0 {
				updPassedTest.AdditionalInfo = append(updPassedTest.AdditionalInfo, additionalInfo)
			}
		}
	}

	return
}

func (w *testWork) toInfo(respStatusCode int) *db.Info {
	return &db.Info{
		Set:                w.setName,
		Case:               w.caseName,
		Payload:            w.payload,
		Encoder:            w.encoder,
		Placeholder:        w.placeholder,
		ResponseStatusCode: respStatusCode,
		Type:               w.testType,
	}
}
