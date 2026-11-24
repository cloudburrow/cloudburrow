package compute

import (
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// Cloud Armor's preconfigured WAF rules (#2160): the rule sets and
// signatures of https://docs.cloud.google.com/armor/docs/waf-rules (read
// 2026-11-22), each signature at its documented ID and sensitivity, with
// detection patterns written here after the OWASP ModSecurity Core Rule
// Set's (Apache-2.0) rules of the same numbers: the request's arguments
// (query and body parameters, names and values), path, cookies and
// headers, URL-decoded and lowercased as CRS transforms them. CRS's
// libinjection rules (942100, 941100) are approximated by token patterns.
// The CRS 4.22 (v422) and 3.0 (no version) rule sets evaluate the same
// patterns as the 3.3 ones, under their own names (docs/gaps.md).
// evaluatePreconfiguredWaf(set, {sensitivity, opt_out_rule_ids,
// opt_in_rule_ids}) evaluates the signatures of sensitivity 1 to the
// given level (4 by default), less those opted out, or only those opted
// in (sensitivity 0); a rule's preconfiguredWafConfig.exclusions leave
// fields out of inspection.

type wafTarget int

const (
	tArgs wafTarget = 1 << iota
	tArgNames
	tPath
	tCookies
	tHeaders
	tUA
	tMethod
	tFilename
	tRawQuery
	tBody
)

type wafSig struct {
	id      string
	sens    int
	targets wafTarget
	re      *regexp.Regexp
	desc    string
}

type wafRuleSet struct {
	family string
	sigs   []wafSig
}

type wafOptions struct {
	sensitivity   int
	optOut, optIn []string
	exclusions    []any
}

func sig(id string, sens int, t wafTarget, pattern string) wafSig {
	return wafSig{id: id, sens: sens, targets: t, re: regexp.MustCompile(pattern)}
}

const crs33 = "owasp-crs-v030301-id"

var (
	argsAll  = tArgs | tArgNames | tCookies
	argsPath = tArgs | tArgNames | tCookies | tPath
)

// wafFamilies are the CRS 3.3 signatures of each rule set family.
var wafFamilies = map[string][]wafSig{
	"sqli": {
		sig(crs33+"942100-sqli", 1, argsAll, `(?:'|")\s*(?:or|and)\s+['"]?\w+['"]?\s*(?:=|like|<|>)|union\s+(?:all\s+)?select|;\s*(?:drop|delete|insert|update|shutdown)\b|(?:'|")\s*;\s*--|\bselect\b.+\bfrom\b|'\s*or\s+'?1'?\s*=\s*'?1`),
		sig(crs33+"942140-sqli", 1, argsAll, `\b(?:information_schema|mysql\.user|pg_catalog|sysobjects|syscolumns|sqlite_master|msdb|tempdb|master\.\.sysdatabases)\b`),
		sig(crs33+"942160-sqli", 1, argsAll, `\b(?:sleep\s*\(\s*\d|benchmark\s*\()`),
		sig(crs33+"942170-sqli", 1, argsAll, `(?:select|;)\s*(?:benchmark|if|sleep)\s*\(\s*\(?\s*\w+`),
		sig(crs33+"942190-sqli", 1, argsAll, `\b(?:exec\s+(?:xp_cmdshell|master\.)|xp_cmdshell|sp_executesql|@@version|union\s+select\s+@@)`),
		sig(crs33+"942220-sqli", 1, argsAll, `^(?:-?2147483648|4294967295|-?9223372036854775808|0x7fffffff)$`),
		sig(crs33+"942230-sqli", 1, argsAll, `\bcase\s+when\b.+\bthen\b|\bif\s*\(\s*\w+\s*[=<>]`),
		sig(crs33+"942240-sqli", 1, argsAll, `alter\s+\w+\s+character\s+set|\bwaitfor\s+delay\b`),
		sig(crs33+"942250-sqli", 1, argsAll, `\bmatch\s*\(.+\)\s*against\s*\(|\bmerge\s+into\b|\bexecute\s+immediate\b`),
		sig(crs33+"942270-sqli", 1, argsAll, `union.*select.*from`),
		sig(crs33+"942280-sqli", 1, argsAll, `\bpg_sleep\s*\(|\bwaitfor\s+delay\s+'|;\s*shutdown\b`),
		sig(crs33+"942290-sqli", 1, argsAll|tRawQuery, `\[\s*\$(?:ne|eq|gt|lt|gte|lte|regex|where|nin|in|exists)\s*\]|\$where\b`),
		sig(crs33+"942320-sqli", 1, argsAll, `\bcreate\s+(?:function|procedure)\b|\bprocedure\s+analyse\b|\bdeclare\s+@\w+`),
		sig(crs33+"942350-sqli", 1, argsAll, `\bcreate\s+function\s+\w+\s+returns\b|;\s*(?:insert|update|delete|drop|alter|create)\s+`),
		sig(crs33+"942360-sqli", 1, argsAll, `\b(?:load_file|into\s+(?:out|dump)file|load\s+data\s+infile)\b`),
		sig(crs33+"942500-sqli", 1, argsAll, `/\*!\d*`),
		sig(crs33+"942110-sqli", 2, argsAll, `(?:^\s*['"]\s*$|'\s*--|"\s*--)`),
		sig(crs33+"942120-sqli", 2, argsAll, `\b(?:rlike|regexp|sounds\s+like|not\s+between|is\s+not\s+null|xor)\b`),
		sig(crs33+"942130-sqli", 2, argsAll, `['"\s]\s*(\w+)\s*['"]?\s*(?:=|<=>|like)\s*['"]?\s*(\w+)`),
		sig(crs33+"942150-sqli", 2, argsAll, `\b(?:ascii|char|concat|substring|substr|version|database|user|schema|hex|unhex|group_concat)\s*\(`),
		sig(crs33+"942180-sqli", 2, argsAll, `['"]\s*(?:or|and)\s*['"]?\d|['"]\s*\|\|\s*['"]`),
		sig(crs33+"942200-sqli", 2, argsAll, `,\s*\(?\s*select|\bselect\s*/\*|`+"`"+`\s*\)`),
		sig(crs33+"942210-sqli", 2, argsAll, `(?:&&|\|\||and|or)\s+\(?\s*select\b`),
		sig(crs33+"942260-sqli", 2, argsAll, `['"]\s*(?:and|or)\s+\w+\s*(?:not\s+)?(?:like|in|between)\b`),
		sig(crs33+"942300-sqli", 2, argsAll, `\)\s*when\s+\d+\s+then|#\s*$|--\s*$`),
		sig(crs33+"942310-sqli", 2, argsAll, `;\s*(?:begin|declare|exec|execute)\b`),
		sig(crs33+"942330-sqli", 2, argsAll, `['"]\s*(?:or|and)\s+['"]?[^'"]+['"]?\s*=\s*['"]?`),
		sig(crs33+"942340-sqli", 2, argsAll, `\bin\s*\(\s*select\b|['"]\s*\)\s*(?:or|and)\s*\(`),
		sig(crs33+"942361-sqli", 2, argsAll, `^\s*(?:alter|union)\b`),
		sig(crs33+"942370-sqli", 2, argsAll, `['"]\s*\*\s*\d|\d\s*[-+*/]\s*['"]`),
		sig(crs33+"942380-sqli", 2, argsAll, `\b(?:having|exists|order\s+by|group\s+by)\b.*\b(?:select|\d)`),
		sig(crs33+"942390-sqli", 2, argsAll, `\b(?:or|and|xor)\s+\d+\s*(?:=|<|>)\s*\d+`),
		sig(crs33+"942400-sqli", 2, argsAll, `\band\s+['"]?\w+['"]?\s*$`),
		sig(crs33+"942410-sqli", 2, argsAll, `\b(?:abs|acos|avg|cast|ceil|coalesce|convert|count|floor|isnull|length|lower|max|min|nullif|round|sum|trim|upper)\s*\(.*\)`),
		sig(crs33+"942470-sqli", 2, argsAll, `\b(?:xp_\w+|sp_\w+|autonomous_transaction|utl_http|utl_inaddr|dbms_\w+)\b`),
		sig(crs33+"942480-sqli", 2, argsAll, `\b(?:select|delete|insert|update)\b.*\b(?:from|into|set|where)\b`),
		sig(crs33+"942430-sqli", 2, tArgs, `(?:[~!@#$%^&*()\-+={}\[\]|:;"'<>].*){12,}`),
		sig(crs33+"942440-sqli", 2, argsAll, `/\*|\*/|--[\s\r\n]|#[\s\r\n]?$`),
		sig(crs33+"942450-sqli", 2, argsAll, `\b0x[0-9a-f]{3,}`),
		sig(crs33+"942510-sqli", 2, argsAll, "`[^`]*`|'[^']*'\\s*(?:or|and)"),
		sig(crs33+"942251-sqli", 3, argsAll, `\bhaving\b\s+(?:\d|'|\w+\s*[=<>])`),
		sig(crs33+"942490-sqli", 3, argsAll, `['"]\s*,\s*['"]?\d|\d\s*,\s*['"]`),
		sig(crs33+"942420-sqli", 3, tCookies, `(?:[~!@#$%^&*()\-+={}\[\]|:;"'<>].*){8,}`),
		sig(crs33+"942431-sqli", 3, tArgs, `(?:[~!@#$%^&*()\-+={}\[\]|:;"'<>].*){6,}`),
		sig(crs33+"942460-sqli", 3, argsAll, `\W{4,}`),
		sig(crs33+"942101-sqli", 3, tPath, `(?:'|")\s*(?:or|and)\s|union\s+select`),
		sig(crs33+"942511-sqli", 3, argsAll, `'[^']*'`),
		sig(crs33+"942421-sqli", 4, tCookies, `(?:[~!@#$%^&*()\-+={}\[\]|:;"'<>].*){3,}`),
		sig(crs33+"942432-sqli", 4, tArgs, `(?:[~!@#$%^&*()\-+={}\[\]|:;"'<>].*){2,}`),
	},
	"xss": {
		sig(crs33+"941100-xss", 1, argsAll|tHeaders, `<\s*script|javascript\s*:|<[^>]+\bon\w+\s*=|<\s*(?:iframe|svg|img|body|object|embed)\b[^>]*>`),
		sig(crs33+"941110-xss", 1, argsAll|tHeaders, `<\s*script[^>]*>`),
		sig(crs33+"941120-xss", 1, argsAll|tHeaders, `[\s"'`+"`"+`;/0-9=]on[a-z]+\s*=`),
		sig(crs33+"941130-xss", 1, argsAll|tHeaders, `[\s\S](?:xmlns|xlink:href|!entity|formaction|@import|;base64)\b`),
		sig(crs33+"941140-xss", 1, argsAll|tHeaders, `(?:javascript|vbscript)\s*:|url\s*\(\s*['"]?\s*javascript`),
		sig(crs33+"941160-xss", 1, argsAll|tHeaders, `<\s*(?:applet|audio|base|bgsound|blink|body|embed|frame|frameset|iframe|ilayer|img|input|isindex|layer|link|marquee|meta|object|script|style|svg|video|xml)\b`),
		sig(crs33+"941170-xss", 1, argsAll|tHeaders, `(?:\bstyle\s*=.*expression\s*\(|\bsrc\s*=\s*['"]?\s*(?:javascript|data):)`),
		sig(crs33+"941180-xss", 1, argsAll, `document\.cookie|document\.write|\.parentnode|\.innerhtml|window\.location|-moz-binding|<!--|-->|<!\[cdata\[`),
		sig(crs33+"941190-xss", 1, argsAll, `<style.*?>.*?(?:@[i\\]|(?:[:=]|&#x?0*(?:58|3a|61|3d);?).*?(?:[(\\]|&#x?0*(?:40|28|92|5c);?))`),
		sig(crs33+"941200-xss", 1, argsAll, `<.*[:]?vmlframe.*?[\s/+]*?src[\s/+]*=`),
		sig(crs33+"941210-xss", 1, argsAll, `(?:j|&#x?0*(?:74|4a|106|6a);?)(?:\t|&tab;)*(?:a|&#x?0*(?:65|41|97|61);?)(?:\t|&tab;)*(?:v|&#x?0*(?:86|56|118|76);?)(?:\t|&tab;)*(?:a|&#x?0*(?:65|41|97|61);?)`),
		sig(crs33+"941220-xss", 1, argsAll, `(?:v|&#x?0*(?:86|56|118|76);?)(?:\t|&tab;)*(?:b|&#x?0*(?:66|42|98|62);?)(?:\t|&tab;)*(?:s|&#x?0*(?:83|53|115|73);?)(?:\t|&tab;)*(?:c|&#x?0*(?:67|43|99|63);?)`),
		sig(crs33+"941230-xss", 1, argsAll, `<embed[\s/+].*?(?:src|type).*?=`),
		sig(crs33+"941240-xss", 1, argsAll, `<[?]?import[\s/+\S]*?implementation[\s/+]*?=`),
		sig(crs33+"941250-xss", 1, argsAll, `<meta[\s/+].*?http-equiv[\s/+]*=[\s/+]*[\"'`+"`"+`]?(?:c|&#x?0*(?:67|43|99|63);?)`),
		sig(crs33+"941260-xss", 1, argsAll, `<meta[\s/+].*?charset[\s/+]*=`),
		sig(crs33+"941270-xss", 1, argsAll, `<link[\s/+].*?href[\s/+]*=`),
		sig(crs33+"941280-xss", 1, argsAll, `<base[\s/+].*?href[\s/+]*=`),
		sig(crs33+"941290-xss", 1, argsAll, `<applet[\s/+>]`),
		sig(crs33+"941300-xss", 1, argsAll, `<object[\s/+].*?(?:type|codetype|classid|code|data)[\s/+]*=`),
		sig(crs33+"941310-xss", 1, argsAll, `\xbc[^\xbe>]*[\xbe>]|<[^\xbe]*\xbe`),
		sig(crs33+"941350-xss", 1, argsAll, `\+adw-|\+ad4-|\+acie-`),
		sig(crs33+"941360-xss", 1, argsAll, `!!\[\]|\[\]\[\(!\[\]\+\[\]\)`),
		sig(crs33+"941370-xss", 1, argsAll, `(?:self|document|this|top|window)\s*(?:/\*|[\[)]).+?(?:\]|\*/)`),
		sig(crs33+"941101-xss", 2, tPath|tHeaders, `<\s*script|javascript:|<[^>]+on\w+\s*=`),
		sig(crs33+"941150-xss", 2, argsAll, `\b(?:dynsrc|datasrc|frameset|ilayer|layer|lowsrc|vmlframe)\b\s*=`),
		sig(crs33+"941320-xss", 2, argsAll, `<(?:a|abbr|acronym|address|applet|area|audioscope|b|base|basefront|bdo|bgsound|big|blackface|blink|blockquote|body|bq|br|button|caption|center|cite|code|col|colgroup|comment|dd|del|dfn|dir|div|dl|dt|em|embed|fieldset|fn|font|form|frame|frameset|h1|head|hr|html|i|iframe|ilayer|img|input|ins|isindex|kdb|keygen|label|layer|legend|li|limittext|link|listing|map|marquee|menu|meta|multicol|nobr|noembed|noframes|noscript|nosmartquotes|object|ol|optgroup|option|p|param|plaintext|pre|q|rt|ruby|s|samp|script|select|server|shadow|sidebar|small|spacer|span|strike|strong|style|sub|sup|table|tbody|td|textarea|tfoot|th|thead|title|tr|tt|u|ul|var|wbr|xml|xmp)\W`),
		sig(crs33+"941330-xss", 2, argsAll, `[\"'][ ]*(?:[^a-z0-9~_:' ]|in).*?(?:(?:l|\\u006c)(?:o|\\u006f)(?:c|\\u0063)(?:a|\\u0061)(?:t|\\u0074)(?:i|\\u0069)(?:o|\\u006f)(?:n|\\u006e)|(?:n|\\u006e)(?:a|\\u0061)(?:m|\\u006d)(?:e|\\u0065)|(?:o|\\u006f)(?:n|\\u006e)(?:e|\\u0065)(?:r|\\u0072)(?:r|\\u0072)(?:o|\\u006f)(?:r|\\u0072)|(?:v|\\u0076)(?:a|\\u0061)(?:l|\\u006c)(?:u|\\u0075)(?:e|\\u0065)(?:o|\\u006f)(?:f|\\u0066)).*?=`),
		sig(crs33+"941340-xss", 2, argsAll, `[\"\'][ ]*(?:[^a-z0-9~_:\' ]|in).+?[.].+?=`),
		sig(crs33+"941380-xss", 2, argsAll, `{{.*?}}`),
	},
	"lfi": {
		sig(crs33+"930100-lfi", 1, tPath|tArgs|tHeaders|tRawQuery, `(?:%2e%2e|%c0%ae%c0%ae|%252e%252e|\.%2e|%2e\.)(?:/|%2f|%5c|\\)`),
		sig(crs33+"930110-lfi", 1, tPath|tArgs|tHeaders, `(?:^|[\\/])\.\.(?:[\\/]|$)`),
		sig(crs33+"930120-lfi", 1, argsPath, `(?:etc/(?:passwd|shadow|group|hosts|issue)|proc/self/|win\.ini|boot\.ini|system32|\.htaccess|\.htpasswd|\.bash_history|\.ssh/|id_rsa|web\.config)`),
		sig(crs33+"930130-lfi", 1, tPath, `(?:/\.git/|/\.svn/|/\.env|/\.aws/|/\.ds_store|/\.hg/|/\.bzr/|/\.idea/|/web-inf/|/meta-inf/)`),
	},
	"rfi": {
		sig(crs33+"931100-rfi", 1, tArgs, `^(?:file|ftps?|https?)://(?:\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3})`),
		sig(crs33+"931110-rfi", 1, tArgs, `(?:\binclude\s*\([^)]*|mosconfig_absolute_path|_conf\[path\]|_server\[document_root\]|gallery|phpbb_root_path)=?(?:file|ftps?|https?)://`),
		sig(crs33+"931120-rfi", 1, tArgs, `^(?:file|ftps?|https?).*?\?+$`),
		sig(crs33+"931130-rfi", 2, tArgs, `^(?:file|ftps?|https?)://[^/]+`),
	},
	"rce": {
		sig(crs33+"932100-rce", 1, argsAll, `(?:;|\||&&|\$\(|`+"`"+`)\s*(?:ls|cat|id|whoami|uname|wget|curl|nc|ncat|bash|sh|zsh|python|perl|ruby|php|chmod|chown|rm|ping|nslookup|telnet)\b`),
		sig(crs33+"932105-rce", 1, argsAll, `(?:;|\||&&)\s*(?:/bin/|/usr/bin/|/sbin/)\w+`),
		sig(crs33+"932110-rce", 1, argsAll, `(?:;|\||&&|%)\s*(?:cmd(?:\.exe)?|net\s+user|ipconfig|systeminfo|tasklist|reg\s+query|certutil|bitsadmin)\b`),
		sig(crs33+"932115-rce", 1, argsAll, `(?:^|[;&|])\s*(?:copy|del|dir|echo|erase|move|ren|type|xcopy)\s+[a-z]:\\`),
		sig(crs33+"932120-rce", 1, argsAll, `\b(?:powershell(?:\.exe)?|invoke-expression|iex|invoke-webrequest|new-object\s+net\.webclient|-encodedcommand|start-process)\b`),
		sig(crs33+"932130-rce", 1, argsAll, `\$\((?:[^)]+)\)|\$\{[^}]+\}|`+"`[^`]+`"),
		sig(crs33+"932140-rce", 1, argsAll, `\b(?:for(?:/[dflr].*)?\s+%+[^ ]+\s+in\s*\(|if(?:/i)?(?:\s+not)?(?:\s+(?:exist|defined|errorlevel|cmdextversion|(?:[^ ]+\s+(?:==|equ|neq|lss|leq|gtr|geq))))\b)`),
		sig(crs33+"932150-rce", 1, argsAll, `^\s*(?:ls|cat|id|whoami|uname|wget|curl|nc|bash|sh|python|perl|php|ruby|ping)(?:\s|$)`),
		sig(crs33+"932160-rce", 1, argsAll, `(?:/bin/(?:ba|z|k|da|c)?sh|/etc/passwd|/dev/tcp/|/usr/bin/(?:perl|python|ruby|php))`),
		sig(crs33+"932170-rce", 1, tHeaders|tUA, `^\(\s*\)\s+\{`),
		sig(crs33+"932171-rce", 1, argsAll, `^\(\s*\)\s+\{`),
		sig(crs33+"932180-rce", 1, tFilename, `(?:\.htaccess|\.htdigest|\.htpasswd|\.bash_profile|\.bashrc|\.profile|\.git|web\.config)$`),
		sig(crs33+"932200-rce", 2, argsAll, `(?:\w*['"\\][\w'"\\]*[^\w\s]|\$\{?ifs\}?|\*\s*$|\?\?)`),
		sig(crs33+"932106-rce", 3, argsAll, `(?:^|[;|&])\s*(?:ls|ps|w|df|du|ip|top|env|set)\s*(?:$|[;|&])`),
		sig(crs33+"932190-rce", 3, argsAll, `/\w*[*?]\w*/|\b\w+[*?]+\w*\s+/`),
	},
	"methodenforcement": {
		sig(crs33+"911100-methodenforcement", 1, tMethod, `^(?:connect|trace|track|propfind|proppatch|mkcol|copy|move|lock|unlock|search|debug)$`),
	},
	"scannerdetection": {
		sig(crs33+"913100-scannerdetection", 1, tUA, `(?:nikto|sqlmap|nmap|nessus|openvas|acunetix|netsparker|w3af|dirbuster|gobuster|wpscan|masscan|zgrab|arachni|skipfish|whatweb|burpsuite|nuclei|havij|zmeu|jaascois|bsqlbf)`),
		sig(crs33+"913110-scannerdetection", 1, tHeaders, `(?:acunetix-product|x-scan-memo|x-wipp|x-request-memo|x-scanner|bytes=0-,5-0,5-1)`),
		sig(crs33+"913120-scannerdetection", 1, tPath|tArgNames, `(?:/nessus_is_probing_you_|/acunetix-wvs-test-for-some-inexistent-file|/w00tw00t\.at\.|/nikto-test|/sqlmap|test_cgi_proxy_url|nessustest|appscan_fingerprint)`),
		sig(crs33+"913101-scannerdetection", 2, tUA, `^(?:curl|wget|python-requests|python-urllib|go-http-client|java/|libwww-perl|lwp-|httpclient|okhttp|php/|ruby|powershell|axios)`),
		sig(crs33+"913102-scannerdetection", 2, tUA, `(?:bot|crawler|spider|scrapy|ahrefs|semrush|mj12bot|dotbot|petalbot|bytespider)`),
	},
	"protocolattack": {
		sig(crs33+"921110-protocolattack", 1, argsAll|tBody, `(?:get|post|head|options|connect|put|delete|trace|track|patch|propfind|propatch|mkcol|copy|move|lock|unlock)\s+[^\s]+\s+http/\d`),
		sig(crs33+"921120-protocolattack", 1, argsAll, `[\r\n]\W*?(?:content-(?:type|length)|set-cookie|location):\s*\w`),
		sig(crs33+"921130-protocolattack", 1, argsAll, `(?:\bhttp/\d|<(?:html|meta)\b)`),
		sig(crs33+"921140-protocolattack", 1, tHeaders, `[\n\r]`),
		sig(crs33+"921150-protocolattack", 1, tArgNames, `[\n\r]`),
		sig(crs33+"921160-protocolattack", 1, tArgs, `[\n\r]+(?:\s|location|refresh|(?:set-)?cookie|(?:x-)?(?:forwarded-(?:for|host|server)|host|via|remote-ip|remote-addr|originating-ip))\s*:`),
		sig(crs33+"921190-protocolattack", 1, tPath, `[\n\r]`),
		sig(crs33+"921200-protocolattack", 1, argsAll, `^[^:\(\)\&\|\!\<\>\~]*\)\s*(?:\((?:[^,\(\)\=\&\|\!\<\>\~]+[><~]?=|\s*[&!|]\s*(?:\)|\()?\s*)|\)\s*\(\s*[\&\|\!]\s*|[&!|]\s*\([^\(\)\=\&\|\!\<\>\~]+[><~]?=[^:\(\)\&\|\!\<\>\~]*)`),
		sig(crs33+"921151-protocolattack", 2, tArgs, `[\n\r]`),
		// HTTP parameter pollution: a parameter given twice (wafMatch;
		// RE2 has no back-references).
		{id: crs33 + "921170-protocolattack", sens: 3, targets: tRawQuery},
	},
	"php": {
		sig(crs33+"933100-php", 1, argsAll, `<\?(?:php|=)?|\?>`),
		sig(crs33+"933110-php", 1, tFilename, `\.(?:php\d*|phtml|phar)\.*$`),
		sig(crs33+"933120-php", 1, argsAll, `\b(?:allow_url_include|auto_prepend_file|auto_append_file|disable_functions|open_basedir|safe_mode|register_globals)\s*=`),
		sig(crs33+"933130-php", 1, argsAll, `\$_(?:get|post|cookie|request|server|session|env|files)\b|\$globals\b|\$http_raw_post_data`),
		sig(crs33+"933140-php", 1, argsAll, `php://(?:std(?:in|out|err)|(?:in|out)put|fd|memory|temp|filter)`),
		sig(crs33+"933200-php", 1, argsAll, `(?:zlib|glob|phar|ssh2|rar|ogg|expect|zip|data)://`),
		sig(crs33+"933150-php", 1, argsAll, `\b(?:base64_decode|call_user_func|create_function|eval|exec|fsockopen|gzinflate|passthru|popen|proc_open|shell_exec|system|assert|preg_replace|str_rot13|move_uploaded_file|phpinfo)\b`),
		sig(crs33+"933160-php", 1, argsAll, `\b(?:eval|exec|system|passthru|shell_exec|assert|popen|proc_open|phpinfo)\s*\(`),
		sig(crs33+"933170-php", 1, argsAll|tBody, `[oc]:\d+:"\w+":\d+:\{`),
		sig(crs33+"933180-php", 1, argsAll, `\$+(?:[a-z_\x7f-\xff][a-z0-9_\x7f-\xff]*|\s*{.+})(?:\s|\[.+\]|{.+}|/\*.*\*/|//.*|#.*)*\(.*\)`),
		sig(crs33+"933210-php", 1, argsAll, `(?:\((?:.+\)(?:[\"'][\-0-9a-z]*[\"'])?(?:.+\(.+\))?)+|[a-z0-9_]+\s*\[\s*[\"']?[a-z0-9_]+[\"']?\s*\]\s*\(.*\)|\$\{.*\}\s*\()`),
		sig(crs33+"933151-php", 2, argsAll, `\b(?:chmod|chown|copy|curl_exec|file_get_contents|file_put_contents|fopen|fwrite|include|include_once|mkdir|readfile|rename|require|require_once|rmdir|symlink|unlink|ini_set|parse_str|extract|putenv)\b`),
		sig(crs33+"933131-php", 3, argsAll, `\$(?:_|globals|http_)\w*`),
		sig(crs33+"933161-php", 3, argsAll, `\b(?:abs|chr|ord|strlen|strrev|implode|explode|array_map|array_filter|usort|serialize|unserialize|print_r|var_dump)\s*\(`),
		sig(crs33+"933111-php", 3, tFilename, `\.(?:php\d*|phtml|phar|phps|pht|inc)`),
		sig(crs33+"933190-php", 3, argsAll, `\?>`),
	},
	"sessionfixation": {
		sig(crs33+"943100-sessionfixation", 1, argsAll, `\.cookie\b.*?;\W*?(?:expires|domain)\W*?=|\bhttp-equiv\W+set-cookie\b`),
		sig(crs33+"943110-sessionfixation", 1, tArgNames, `^(?:jsessionid|aspsessionid|asp\.net_sessionid|phpsession|phpsessid|weblogicsession|session_id|session-id|cfid|cftoken|cfsid|jservsession|jwsession)$`),
		sig(crs33+"943120-sessionfixation", 1, tArgNames, `^(?:jsessionid|aspsessionid|asp\.net_sessionid|phpsession|phpsessid|weblogicsession|session_id|session-id|cfid|cftoken|cfsid|jservsession|jwsession)$`),
	},
	"java": {
		sig(crs33+"944100-java", 1, argsAll|tHeaders|tBody, `java\.lang\.(?:runtime|processbuilder)`),
		sig(crs33+"944110-java", 1, argsAll|tBody, `(?:runtime|processbuilder).*(?:unmarshaller|base64data|java\.)`),
		sig(crs33+"944120-java", 1, argsAll|tBody, `(?:clonetransformer|forclosure|instantiatefactory|instantiatetransformer|invokertransformer|prototypeclonefactory|prototypeserializationfactory|whileclosure|getproperty|filewriter|xmldecoder)`),
		sig(crs33+"944130-java", 1, argsAll|tBody, `\b(?:com\.opensymphony\.xwork2|com\.sun\.org\.apache|java\.io\.(?:bufferedinputstream|bufferedreader|fileinputstream|fileoutputstream|filereader)|java\.lang\.(?:class|classloader|reflect|system|thread)|javax\.script\.scriptenginemanager|org\.apache\.commons|org\.springframework)\b`),
		sig(crs33+"944200-java", 2, argsAll|tBody, `\xac\xed\x00\x05`),
		sig(crs33+"944210-java", 2, argsAll|tBody|tHeaders, `(?:ro0ab|ka0ab|apuwbq|rO0AB)`),
		sig(crs33+"944240-java", 2, argsAll|tBody, `(?:clonetransformer|forclosure|instantiatefactory|instantiatetransformer|invokertransformer|prototypeclonefactory|prototypeserializationfactory|whileclosure)`),
		sig(crs33+"944250-java", 2, argsAll|tBody, `java\b.+(?:runtime|processbuilder)`),
		sig(crs33+"944300-java", 3, argsAll|tBody, `(?:cnvudgltzq|hjvbnrpbw|zxhly|cnvudgltz|uhjvy2vzc0j1awxkzx|anvudgltzq)`),
	},
	"nodejs": {
		sig(crs33+"934100-nodejs", 1, argsAll|tBody, `(?:_(?:\$\$nd_func|_js_function)|(?:\beval|new\s*function)\s*\(|string\s*\.\s*fromcharcode|function\s*\(\s*\)\s*{|this\.constructor|module\.exports\s*=|\([\s\S]*?\)\s*=>\s*{|require\s*\(\s*['"]child_process)`),
	},
}

// wafCVE is cve-canary's signatures.
var wafCVE = []wafSig{
	sig("owasp-crs-v030001-id044228-cve", 1, argsAll|tHeaders|tUA|tPath, `\$\{\s*jndi\s*:\s*(?:ldap|ldaps|rmi|dns|iiop|nis|nds|corba|http)`),
	sig("owasp-crs-v030001-id144228-cve", 1, argsAll|tHeaders|tUA|tPath, `\$\{[^}]*\$\{|\$\{\s*(?:lower|upper|env|sys|date|::-[a-z])`),
	sig("owasp-crs-v030001-id244228-cve", 3, argsAll|tHeaders|tUA|tPath, `\$\{`),
	sig("owasp-crs-v030001-id344228-cve", 3, argsAll|tHeaders|tUA|tPath, `(?:jhtqbmrp|r7jndi|ckey7am5kaq|jhtqbmrpo)`),
	sig("google-mrs-v202512-id000001-rce", 0, tBody|tHeaders, `"\$\$typeof"|\bresolved_model\b|"_response"\s*:`),
	sig("google-mrs-v202512-id000002-rce", 0, tBody|tHeaders, `__proto__|"then"\s*:\s*"\$`),
}

// wafJSONSQLi is json-sqli-canary's signature.
var wafJSONSQLi = []wafSig{
	sig("owasp-crs-id942550-sqli", 2, argsAll|tBody, `[\"'`+"`"+`][\[\{].*[\]\}][\"'`+"`"+`].*(?:::.*jsonb?)?.*(?:(?:@|->?)>|<@|\?[&|]?|#>>?|[<>]|<-)|(?:(?:@|->?)>|<@|\?[&|]?|#>>?|[<>]|<-)[\"'`+"`"+`][\[\{].*[\]\}][\"'`+"`"+`]|json_extract.*\(.*\)`),
}

var wafVersions = []string{"-v422-stable", "-v422-canary", "-v33-stable", "-v33-canary", "-stable", "-canary"}

// wafSet is a rule set by name, nil when Google has none of that name.
func wafSet(name string) *wafRuleSet {
	switch name {
	case "cve-canary":
		return &wafRuleSet{family: "cve", sigs: wafCVE}
	case "json-sqli-canary":
		return &wafRuleSet{family: "json-sqli", sigs: wafJSONSQLi}
	}
	for _, v := range wafVersions {
		if !strings.HasSuffix(name, v) {
			continue
		}
		fam := strings.TrimSuffix(name, v)
		if fam == "generic" && strings.HasPrefix(v, "-v422") {
			return &wafRuleSet{family: fam, sigs: wafFamilies["nodejs"]}
		}
		if fam == "nodejs" && !strings.HasPrefix(v, "-v33") || fam == "java" && !strings.HasPrefix(v, "-v") {
			return nil
		}
		if sigs, ok := wafFamilies[fam]; ok {
			return &wafRuleSet{family: fam, sigs: sigs}
		}
	}
	return nil
}

// preconfiguredExpressionSets is securityPolicies.listPreconfiguredExpressionSets.
func preconfiguredExpressionSets() obj {
	var names []string
	for fam := range wafFamilies {
		for _, v := range wafVersions {
			if wafSet(fam+v) != nil {
				names = append(names, fam+v)
			}
		}
	}
	names = append(names, "generic-v422-stable", "generic-v422-canary", "cve-canary", "json-sqli-canary")
	sort.Strings(names)
	var sets []any
	for _, n := range names {
		rs := wafSet(n)
		var exprs []any
		for _, sg := range rs.sigs {
			exprs = append(exprs, obj{"id": sg.id, "sensitivity": float64(sg.sens)})
		}
		e := obj{"id": n, "expressions": exprs}
		if strings.HasSuffix(n, "-v33-stable") {
			e["aliases"] = []any{strings.TrimSuffix(n, "-v33-stable") + "-v33-canary"}
		}
		sets = append(sets, e)
	}
	return obj{"preconfiguredExpressionSets": obj{"wafRules": obj{"expressionSets": sets}}}
}

// wafMatch evaluates a rule set against the request.
func (a *armorReq) wafMatch(name string, o wafOptions) bool {
	rs := wafSet(name)
	if rs == nil {
		return false
	}
	in := a.wafInputs(o.exclusions, name)
	for _, sg := range rs.sigs {
		switch {
		case len(o.optIn) > 0:
			if !hasStr(o.optIn, sg.id) {
				continue
			}
		case sg.sens == 0 || sg.sens > o.sensitivity:
			continue
		case hasStr(o.optOut, sg.id):
			continue
		}
		if sg.re == nil {
			if sg.id == crs33+"921170-protocolattack" && repeatedParam(a.r.URL.RawQuery) {
				return true
			}
			continue
		}
		for t, vals := range in {
			if sg.targets&t == 0 {
				continue
			}
			for _, v := range vals {
				if sg.re.MatchString(v) {
					return true
				}
			}
		}
	}
	return false
}

// wafInputs are the request's inspected fields by target, decoded and
// lowercased; excluded fields are left out.
func (a *armorReq) wafInputs(exclusions []any, set string) map[wafTarget][]string {
	excluded := func(kind, name string) bool {
		for _, e := range exclusions {
			ex := objOf(e)
			if str(ex, "targetRuleSet") != "" && str(ex, "targetRuleSet") != set {
				continue
			}
			for _, f := range listOf(ex["requestQueryParamsToExclude"]) {
				if kind == "arg" && fieldMatch(objOf(f), name) {
					return true
				}
			}
			for _, f := range listOf(ex["requestCookiesToExclude"]) {
				if kind == "cookie" && fieldMatch(objOf(f), name) {
					return true
				}
			}
			for _, f := range listOf(ex["requestHeadersToExclude"]) {
				if kind == "header" && fieldMatch(objOf(f), name) {
					return true
				}
			}
			for _, f := range listOf(ex["requestUrisToExclude"]) {
				if kind == "uri" && fieldMatch(objOf(f), name) {
					return true
				}
			}
		}
		return false
	}
	norm := func(s string) string { return strings.ToLower(armorURLDecodeUni(s)) }
	in := map[wafTarget][]string{}
	r := a.r
	if !excluded("uri", r.URL.Path) {
		in[tPath] = []string{norm(r.URL.EscapedPath()), strings.ToLower(r.URL.EscapedPath())}
		in[tFilename] = []string{norm(r.URL.Path)}
		in[tRawQuery] = []string{strings.ToLower(r.URL.RawQuery), norm(r.URL.RawQuery)}
	}
	in[tMethod] = []string{strings.ToLower(r.Method)}
	in[tUA] = []string{strings.ToLower(r.UserAgent())}
	params := a.params
	if params == nil {
		params = map[string]string{}
		q, _ := url.ParseQuery(r.URL.RawQuery)
		for k, v := range q {
			params[k] = strings.Join(v, ",")
		}
	}
	for k, v := range params {
		if excluded("arg", k) {
			continue
		}
		in[tArgs] = append(in[tArgs], norm(v))
		in[tArgNames] = append(in[tArgNames], norm(k))
	}
	for _, c := range r.Cookies() {
		if !excluded("cookie", c.Name) {
			in[tCookies] = append(in[tCookies], norm(c.Value))
		}
	}
	for k, vs := range r.Header {
		lk := strings.ToLower(k)
		if lk == "cookie" || excluded("header", lk) {
			continue
		}
		for _, v := range vs {
			in[tHeaders] = append(in[tHeaders], strings.ToLower(v))
		}
	}
	if a.body != nil {
		in[tBody] = []string{strings.ToLower(string(a.body))}
	}
	return in
}

// fieldMatch is an exclusion's field operator (EQUALS, STARTS_WITH,
// ENDS_WITH, CONTAINS, EQUALS_ANY).
func fieldMatch(f obj, name string) bool {
	v := strings.ToLower(str(f, "val"))
	name = strings.ToLower(name)
	switch str(f, "op") {
	case "EQUALS":
		return name == v
	case "STARTS_WITH":
		return strings.HasPrefix(name, v)
	case "ENDS_WITH":
		return strings.HasSuffix(name, v)
	case "CONTAINS":
		return strings.Contains(name, v)
	case "EQUALS_ANY":
		return true
	}
	return false
}

// repeatedParam is whether a raw query names a parameter twice.
func repeatedParam(q string) bool {
	seen := map[string]bool{}
	for _, kv := range strings.Split(q, "&") {
		k, _, _ := strings.Cut(kv, "=")
		if k == "" {
			continue
		}
		if seen[k] {
			return true
		}
		seen[k] = true
	}
	return false
}
