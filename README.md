# Go Test WAF

An open-source Go project to test different WAF for detection logic and bypasses.

# How it works

It is a 3-steps requests generation process that multiply amount of payloads to encoders and placeholders. Let's say you defined 2 payloads, 3 encoders (Base64, JSON, and URLencode) and 1 placeholder (HTTP GET variable). In this case, the tool will send 2x3x1 = 6 requests in a testcase.

## Payload

The payload string you wanna send. Like ```<script>alert(111)</script>``` or something more sophisticated. There is no macroses like so far, but it's in our TODO list. Since it's a YAML string, use binary encoding if you wanna to https://yaml.org/type/binary.html

## Encoder

Data encoder the tool should apply to the payload. Base64, JSON unicode (\u0027 instead of '), etc.

## Placeholder

A place inside HTTP request where encoded payload should be. Like URL parameter, URI, POST form parameter, or JSON POST body.

# Quick start
```
docker build . --force-rm -t gotestwaf
docker run -v /tmp:/tmp/report gotestwaf --url=https://the-waf-you-wanna-test/
```

Find the report file waf-test-report-<date>.pdf in a /tmp folder you mapped to /tmp/report inside the container.


# Examples

## Testing on OWASP ModSecurity Core Rule Set

#### Build & run ModSecurity CRS docker image
```
git clone https://github.com/SpiderLabs/owasp-modsecurity-crs
cd owasp-modsecurity-crs/util/docker
docker build -t modsec_crs --file Dockerfile-3.0-nginx .
docker run --rm -p 8080:80 -e PARANOIA=1 modsec_crs
```

You may choose the PARANOIA level to increase the level of security.  
Learn more https://coreruleset.org/faq/

#### Run gotestwaf
`docker run -v /tmp:/tmp/report gotestwaf --url=http://the-waf-you-wanna-test/`

#### Check results
```
owasp ss-include  5/20  (0.25)
owasp xml-injection 12/12 (1.00)
owasp xss-scripting 9/28  (0.32)
owasp ldap-injection  0/8 (0.00)
owasp mail-injection  3/12  (0.25)
owasp nosql-injection 0/18  (0.00)
owasp path-traversal  8/24  (0.33)
owasp shell-injection 3/8 (0.38)
owasp sql-injection 8/32  (0.25)
owasp sst-injection 5/20  (0.25)
owasp-api graphql 1/1 (1.00)
owasp-api rest  2/2 (1.00)
owasp-api soap  0/2 (0.00)
false-pos texts 7/8 (0.88)

WAF score: 42.18%
132 bypasses in 195 tests / 14 test cases
```
---
