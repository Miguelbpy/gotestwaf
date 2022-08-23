package db

import (
	"math"
	"sort"
	"strings"
)

type Statistics struct {
	IsGrpcAvailable bool

	Paths ScannedPaths

	TestCasesFingerprint string

	SummaryTable []*SummaryTableRow
	Blocked      []*TestDetails
	Bypasses     []*TestDetails
	Unresolved   []*TestDetails
	Failed       []*FailedDetails

	PositiveTests struct {
		SummaryTable  []*SummaryTableRow
		FalsePositive []*TestDetails
		TruePositive  []*TestDetails
		Unresolved    []*TestDetails
		Failed        []*FailedDetails

		AllRequestsNumber        int
		BlockedRequestsNumber    int
		BypassedRequestsNumber   int
		UnresolvedRequestsNumber int
		FailedRequestsNumber     int
		ResolvedRequestsNumber   int

		UnresolvedRequestsPercentage    float64
		ResolvedFalseRequestsPercentage float64
		ResolvedTrueRequestsPercentage  float64
		FailedRequestsPercentage        float64
	}

	AllRequestsNumber        int
	BlockedRequestsNumber    int
	BypassedRequestsNumber   int
	UnresolvedRequestsNumber int
	FailedRequestsNumber     int
	ResolvedRequestsNumber   int

	UnresolvedRequestsPercentage       float64
	ResolvedBlockedRequestsPercentage  float64
	ResolvedBypassedRequestsPercentage float64
	FailedRequestsPercentage           float64

	OverallRequests int
	WafScore        float64
}

type SummaryTableRow struct {
	TestSet    string
	TestCase   string
	Percentage float64
	Sent       int
	Blocked    int
	Bypassed   int
	Unresolved int
	Failed     int
}

type TestDetails struct {
	Payload            string
	TestCase           string
	TestSet            string
	Encoder            string
	Placeholder        string
	ResponseStatusCode int
	AdditionalInfo     []string
	Type               string
}

type FailedDetails struct {
	Payload     string
	TestCase    string
	TestSet     string
	Encoder     string
	Placeholder string
	Reason      []string
	Type        string
}

type Path struct {
	Method string
	Path   string
}

type ScannedPaths []*Path

var _ sort.Interface = (ScannedPaths)(nil)

func (sp ScannedPaths) Len() int {
	return len(sp)
}

func (sp ScannedPaths) Less(i, j int) bool {
	if sp[i].Path > sp[j].Path {
		return false
	} else if sp[i].Path < sp[j].Path {
		return true
	}

	return sp[i].Method < sp[j].Method
}

func (sp ScannedPaths) Swap(i, j int) {
	sp[i], sp[j] = sp[j], sp[i]
}

func (sp ScannedPaths) Sort() {
	sort.Sort(sp)
}

func Round(n float64) float64 {
	return math.Round(n*100) / 100
}

func CalculatePercentage(first, second int) float64 {
	if second == 0 {
		return 0.0
	}
	result := float64(first) / float64(second) * 100
	return Round(result)
}

func isPositiveTest(setName string) bool {
	return strings.Contains(setName, "false")
}

func (db *DB) GetStatistics(ignoreUnresolved, nonBlockedAsPassed bool) *Statistics {
	db.Lock()
	defer db.Unlock()

	s := &Statistics{
		IsGrpcAvailable:      db.IsGrpcAvailable,
		TestCasesFingerprint: db.Hash,
	}

	unresolvedRequestsNumber := make(map[string]map[string]int)

	for _, unresolvedTest := range db.naTests {
		if unresolvedRequestsNumber[unresolvedTest.Set] == nil {
			unresolvedRequestsNumber[unresolvedTest.Set] = make(map[string]int)
		}

		// If we want to count UNRESOLVED as BYPASSED, we shouldn't count UNRESOLVED at all
		// set it to zero by default
		if ignoreUnresolved || nonBlockedAsPassed {
			unresolvedRequestsNumber[unresolvedTest.Set][unresolvedTest.Case] = 0
		} else {
			unresolvedRequestsNumber[unresolvedTest.Set][unresolvedTest.Case]++
		}
	}

	var overallCompletedTestCases int
	var overallPassedRequestsPercentage float64

	// Sort all test sets by name
	var sortedTestSets []string
	for testSet := range db.counters {
		sortedTestSets = append(sortedTestSets, testSet)
	}
	sort.Strings(sortedTestSets)

	for _, testSet := range sortedTestSets {
		// Sort all test cases by name
		var sortedTestCases []string
		for testCase := range db.counters[testSet] {
			sortedTestCases = append(sortedTestCases, testCase)
		}
		sort.Strings(sortedTestCases)

		isPositive := isPositiveTest(testSet)

		for _, testCase := range sortedTestCases {
			// Number of requests for all request types for the selected testCase
			unresolvedRequests := unresolvedRequestsNumber[testSet][testCase]
			passedRequests := db.counters[testSet][testCase]["passed"]
			blockedRequests := db.counters[testSet][testCase]["blocked"]
			failedRequests := db.counters[testSet][testCase]["failed"]
			totalRequests := passedRequests + blockedRequests + failedRequests

			// If we don't want to count UNRESOLVED requests as BYPASSED, we need to subtract them
			// from blocked requests (in other case we will count them as usual), and add this
			// subtracted value to the overall requests
			if !ignoreUnresolved || !nonBlockedAsPassed {
				blockedRequests -= unresolvedRequests
			}

			totalResolvedRequests := passedRequests + blockedRequests

			s.OverallRequests += totalRequests

			row := &SummaryTableRow{
				TestSet:    testSet,
				TestCase:   testCase,
				Percentage: 0.0,
				Sent:       totalRequests,
				Blocked:    blockedRequests,
				Bypassed:   passedRequests,
				Unresolved: unresolvedRequests,
				Failed:     failedRequests,
			}

			// If positive set - move to another table (remove from general cases)
			if isPositive {
				// False positive - blocked by the WAF (bad behavior, blockedRequests)
				s.PositiveTests.BlockedRequestsNumber += blockedRequests
				// True positive - bypassed (good behavior, passedRequests)
				s.PositiveTests.BypassedRequestsNumber += passedRequests
				s.PositiveTests.UnresolvedRequestsNumber += unresolvedRequests
				s.PositiveTests.FailedRequestsNumber += failedRequests

				passedRequestsPercentage := CalculatePercentage(passedRequests, totalResolvedRequests)
				row.Percentage = passedRequestsPercentage

				s.PositiveTests.SummaryTable = append(s.PositiveTests.SummaryTable, row)
			} else {
				s.BlockedRequestsNumber += blockedRequests
				s.BypassedRequestsNumber += passedRequests
				s.UnresolvedRequestsNumber += unresolvedRequests
				s.FailedRequestsNumber += failedRequests

				blockedRequestsPercentage := CalculatePercentage(blockedRequests, totalResolvedRequests)
				row.Percentage = blockedRequestsPercentage

				s.SummaryTable = append(s.SummaryTable, row)

				if totalResolvedRequests != 0 {
					overallCompletedTestCases++
					overallPassedRequestsPercentage += blockedRequestsPercentage
				}
			}
		}
	}

	if overallCompletedTestCases != 0 {
		s.WafScore = Round(overallPassedRequestsPercentage / float64(overallCompletedTestCases))
	}

	// Number of all negative requests
	s.AllRequestsNumber = s.BlockedRequestsNumber +
		s.BypassedRequestsNumber +
		s.UnresolvedRequestsNumber +
		s.FailedRequestsNumber

	// Number of negative resolved requests
	s.ResolvedRequestsNumber = s.BlockedRequestsNumber +
		s.BypassedRequestsNumber

	// Number of all negative requests
	s.PositiveTests.AllRequestsNumber = s.PositiveTests.BlockedRequestsNumber +
		s.PositiveTests.BypassedRequestsNumber +
		s.PositiveTests.UnresolvedRequestsNumber +
		s.PositiveTests.FailedRequestsNumber

	// Number of positive resolved requests
	s.PositiveTests.ResolvedRequestsNumber = s.PositiveTests.BlockedRequestsNumber +
		s.PositiveTests.BypassedRequestsNumber

	s.UnresolvedRequestsPercentage = CalculatePercentage(s.UnresolvedRequestsNumber, s.AllRequestsNumber)
	s.ResolvedBlockedRequestsPercentage = CalculatePercentage(s.BlockedRequestsNumber, s.ResolvedRequestsNumber)
	s.ResolvedBypassedRequestsPercentage = CalculatePercentage(s.BypassedRequestsNumber, s.ResolvedRequestsNumber)
	s.FailedRequestsPercentage = CalculatePercentage(s.FailedRequestsNumber, s.AllRequestsNumber)

	s.PositiveTests.UnresolvedRequestsPercentage = CalculatePercentage(s.PositiveTests.UnresolvedRequestsNumber, s.PositiveTests.AllRequestsNumber)
	s.PositiveTests.ResolvedFalseRequestsPercentage = CalculatePercentage(s.PositiveTests.BlockedRequestsNumber, s.PositiveTests.ResolvedRequestsNumber)
	s.PositiveTests.ResolvedTrueRequestsPercentage = CalculatePercentage(s.PositiveTests.BypassedRequestsNumber, s.PositiveTests.ResolvedRequestsNumber)
	s.PositiveTests.FailedRequestsPercentage = CalculatePercentage(s.PositiveTests.FailedRequestsNumber, s.PositiveTests.AllRequestsNumber)

	for _, blockedTest := range db.blockedTests {
		sort.Strings(blockedTest.AdditionalInfo)

		testDetails := &TestDetails{
			Payload:            blockedTest.Payload,
			TestCase:           blockedTest.Case,
			TestSet:            blockedTest.Set,
			Encoder:            blockedTest.Encoder,
			Placeholder:        blockedTest.Placeholder,
			ResponseStatusCode: blockedTest.ResponseStatusCode,
			AdditionalInfo:     blockedTest.AdditionalInfo,
			Type:               blockedTest.Type,
		}

		if isPositiveTest(blockedTest.Set) {
			s.PositiveTests.FalsePositive = append(s.PositiveTests.FalsePositive, testDetails)
		} else {
			s.Blocked = append(s.Blocked, testDetails)
		}
	}

	for _, passedTest := range db.passedTests {
		sort.Strings(passedTest.AdditionalInfo)

		testDetails := &TestDetails{
			Payload:            passedTest.Payload,
			TestCase:           passedTest.Case,
			TestSet:            passedTest.Set,
			Encoder:            passedTest.Encoder,
			Placeholder:        passedTest.Placeholder,
			ResponseStatusCode: passedTest.ResponseStatusCode,
			AdditionalInfo:     passedTest.AdditionalInfo,
			Type:               passedTest.Type,
		}

		if isPositiveTest(passedTest.Set) {
			s.PositiveTests.TruePositive = append(s.PositiveTests.TruePositive, testDetails)
		} else {
			s.Bypasses = append(s.Bypasses, testDetails)
		}
	}

	for _, unresolvedTest := range db.naTests {
		sort.Strings(unresolvedTest.AdditionalInfo)

		testDetails := &TestDetails{
			Payload:            unresolvedTest.Payload,
			TestCase:           unresolvedTest.Case,
			TestSet:            unresolvedTest.Set,
			Encoder:            unresolvedTest.Encoder,
			Placeholder:        unresolvedTest.Placeholder,
			ResponseStatusCode: unresolvedTest.ResponseStatusCode,
			AdditionalInfo:     unresolvedTest.AdditionalInfo,
			Type:               unresolvedTest.Type,
		}

		if ignoreUnresolved || nonBlockedAsPassed {
			if isPositiveTest(unresolvedTest.Set) {
				s.PositiveTests.FalsePositive = append(s.PositiveTests.FalsePositive, testDetails)
			} else {
				s.Bypasses = append(s.Bypasses, testDetails)
			}
		} else {
			if isPositiveTest(unresolvedTest.Set) {
				s.PositiveTests.Unresolved = append(s.PositiveTests.Unresolved, testDetails)
			} else {
				s.Unresolved = append(s.Unresolved, testDetails)
			}
		}
	}

	for _, failedTest := range db.failedTests {
		testDetails := &FailedDetails{
			Payload:     failedTest.Payload,
			TestCase:    failedTest.Case,
			TestSet:     failedTest.Set,
			Encoder:     failedTest.Encoder,
			Placeholder: failedTest.Placeholder,
			Reason:      failedTest.AdditionalInfo,
			Type:        failedTest.Type,
		}

		if isPositiveTest(failedTest.Set) {
			s.PositiveTests.Failed = append(s.PositiveTests.Failed, testDetails)
		} else {
			s.Failed = append(s.Failed, testDetails)
		}
	}

	if db.scannedPaths != nil {
		var paths ScannedPaths
		for path, methods := range db.scannedPaths {
			for method := range methods {
				paths = append(paths, &Path{
					Method: method,
					Path:   path,
				})
			}
		}

		paths.Sort()

		s.Paths = paths
	}

	return s
}
