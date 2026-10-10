package ipp

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// iosReplay is the operation sequence an iPhone used for a successful label
// print (attribute sets copied from its requests), followed by a Print-Job.
const iosReplay = `
{
	NAME "Get-Printer-Attributes all,media-col-database"
	OPERATION Get-Printer-Attributes
	GROUP operation-attributes-tag
	ATTR charset attributes-charset utf-8
	ATTR naturalLanguage attributes-natural-language en
	ATTR uri printer-uri $uri
	ATTR keyword requested-attributes all,media-col-database
	STATUS successful-ok
	EXPECT printer-kind OF-TYPE keyword WITH-ALL-VALUES "/^(label|roll)$/" COUNT 2
	EXPECT media-col-database OF-TYPE collection COUNT 3
	EXPECT media-col-ready OF-TYPE collection COUNT 1
	EXPECT media-col-ready/media-size/x-dimension WITH-VALUE 6200
	EXPECT media-ready OF-TYPE keyword WITH-VALUE "roll_current_62x0mm" COUNT 1
	EXPECT urf-supported WITH-VALUE "RS300"
	EXPECT printer-uri-supported OF-TYPE uri COUNT 2
	EXPECT uri-security-supported SAME-COUNT-AS printer-uri-supported
	EXPECT printer-input-tray OF-TYPE octetString
	EXPECT printer-geo-location OF-TYPE unknown
}
{
	NAME "Get-Printer-Attributes iOS capability set"
	OPERATION Get-Printer-Attributes
	GROUP operation-attributes-tag
	ATTR charset attributes-charset utf-8
	ATTR naturalLanguage attributes-natural-language en
	ATTR uri printer-uri $uri
	ATTR keyword requested-attributes copies-supported,document-format-supported,document-password-supported,finishings-col-database,finishings-supported,output-bin-default,printer-output-tray,output-bin-supported,identify-actions-supported,job-account-id-supported,jpeg-features-supported,jpeg-k-octets-supported,jpeg-x-dimension-supported,jpeg-y-dimension-supported,landscape-orientation-requested-preferred,media-col-supported,media-source-supported,media-col-ready,operations-supported,pdf-k-octets-supported,print-color-mode-supported,print-quality-supported,print-scaling-supported,printer-charge-info-uri,printer-input-tray,media-type-supported,printer-mandatory-job-attributes,printer-state-reasons,sides-supported,job-presets-supported,printer-more-info,printer-supply-info-uri,printer-strings-uri
	STATUS successful-ok
	EXPECT copies-supported OF-TYPE rangeOfInteger
	EXPECT document-format-supported WITH-VALUE "image/urf"
	EXPECT media-col-ready OF-TYPE collection
	EXPECT media-type-supported OF-TYPE name WITH-VALUE "roll"
	EXPECT operations-supported WITH-VALUE 0x0005
	EXPECT print-quality-supported OF-TYPE enum WITH-ALL-VALUES 4
	EXPECT printer-state-reasons WITH-VALUE "none"
	EXPECT printer-more-info OF-TYPE uri
	EXPECT !media-col-database
	EXPECT !printer-mandatory-job-attributes
	EXPECT !printer-uuid
}
{
	NAME "Get-Printer-Attributes iOS state poll"
	OPERATION Get-Printer-Attributes
	GROUP operation-attributes-tag
	ATTR charset attributes-charset utf-8
	ATTR naturalLanguage attributes-natural-language en
	ATTR uri printer-uri $uri
	ATTR keyword requested-attributes printer-state-reasons,media-source-supported,printer-input-tray,printer-mandatory-job-attributes
	STATUS successful-ok
	EXPECT media-source-supported WITH-VALUE "main"
	EXPECT printer-input-tray
	EXPECT printer-state-reasons
	EXPECT !printer-state
}
{
	NAME "Get-Printer-Attributes without requested-attributes"
	OPERATION Get-Printer-Attributes
	GROUP operation-attributes-tag
	ATTR charset attributes-charset utf-8
	ATTR naturalLanguage attributes-natural-language en
	ATTR uri printer-uri $uri
	STATUS successful-ok
	EXPECT printer-state WITH-VALUE 3
	EXPECT !media-col-database
}
{
	NAME "Validate-Job roll media-col, fidelity"
	OPERATION Validate-Job
	GROUP operation-attributes-tag
	ATTR charset attributes-charset utf-8
	ATTR naturalLanguage attributes-natural-language en
	ATTR uri printer-uri $uri
	ATTR name requesting-user-name mobile
	ATTR mimeMediaType document-format image/urf
	ATTR boolean ipp-attribute-fidelity true
	GROUP job-attributes-tag
	ATTR collection media-col { MEMBER collection media-size { MEMBER integer x-dimension 6200 MEMBER integer y-dimension 1670 } MEMBER integer media-right-margin 152 MEMBER integer media-left-margin 152 MEMBER integer media-top-margin 0 MEMBER integer media-bottom-margin 0 }
	ATTR enum orientation-requested 3
	ATTR keyword print-color-mode monochrome
	ATTR enum print-quality 4
	STATUS successful-ok
	EXPECT !media-col
}
{
	NAME "Validate-Job preset length, fidelity"
	OPERATION Validate-Job
	GROUP operation-attributes-tag
	ATTR charset attributes-charset utf-8
	ATTR naturalLanguage attributes-natural-language en
	ATTR uri printer-uri $uri
	ATTR name requesting-user-name mobile
	ATTR mimeMediaType document-format image/urf
	ATTR boolean ipp-attribute-fidelity true
	GROUP job-attributes-tag
	ATTR collection media-col { MEMBER collection media-size { MEMBER integer x-dimension 6200 MEMBER integer y-dimension 2900 } MEMBER keyword media-source main MEMBER name media-type roll }
	STATUS successful-ok
}
{
	NAME "Validate-Job A4, fidelity"
	OPERATION Validate-Job
	GROUP operation-attributes-tag
	ATTR charset attributes-charset utf-8
	ATTR naturalLanguage attributes-natural-language en
	ATTR uri printer-uri $uri
	ATTR name requesting-user-name mobile
	ATTR mimeMediaType document-format image/urf
	ATTR boolean ipp-attribute-fidelity true
	GROUP job-attributes-tag
	ATTR collection media-col { MEMBER collection media-size { MEMBER integer x-dimension 21000 MEMBER integer y-dimension 29700 } }
	STATUS client-error-attributes-or-values-not-supported
	EXPECT media-col IN-GROUP unsupported-attributes-tag
}
{
	NAME "Validate-Job A4, no fidelity"
	OPERATION Validate-Job
	GROUP operation-attributes-tag
	ATTR charset attributes-charset utf-8
	ATTR naturalLanguage attributes-natural-language en
	ATTR uri printer-uri $uri
	ATTR name requesting-user-name mobile
	ATTR mimeMediaType document-format image/urf
	GROUP job-attributes-tag
	ATTR collection media-col { MEMBER collection media-size { MEMBER integer x-dimension 21000 MEMBER integer y-dimension 29700 } }
	STATUS successful-ok-ignored-or-substituted-attributes
	EXPECT media-col IN-GROUP unsupported-attributes-tag
}
{
	NAME "Get-Printer-Attributes before Create-Job"
	OPERATION Get-Printer-Attributes
	GROUP operation-attributes-tag
	ATTR charset attributes-charset utf-8
	ATTR naturalLanguage attributes-natural-language en
	ATTR uri printer-uri $uri
	ATTR name requesting-user-name mobile
	ATTR keyword requested-attributes compression-supported,job-presets-supported,operations-supported,printer-is-accepting-jobs,printer-state-reasons,printer-state-message
	STATUS successful-ok
	EXPECT compression-supported WITH-VALUE "none"
	EXPECT printer-is-accepting-jobs WITH-VALUE true
	EXPECT printer-state-message
}
{
	NAME "Create-Job roll media-col, fidelity"
	OPERATION Create-Job
	GROUP operation-attributes-tag
	ATTR charset attributes-charset utf-8
	ATTR naturalLanguage attributes-natural-language en
	ATTR uri printer-uri $uri
	ATTR name requesting-user-name mobile
	ATTR name job-name "Labels #26"
	ATTR boolean ipp-attribute-fidelity true
	GROUP job-attributes-tag
	ATTR collection media-col { MEMBER collection media-size { MEMBER integer x-dimension 6200 MEMBER integer y-dimension 1670 } MEMBER integer media-right-margin 152 MEMBER integer media-left-margin 152 MEMBER integer media-top-margin 0 MEMBER integer media-bottom-margin 0 }
	ATTR enum orientation-requested 3
	ATTR keyword print-color-mode monochrome
	ATTR enum print-quality 4
	STATUS successful-ok
	EXPECT job-id OF-TYPE integer WITH-VALUE >0
	EXPECT job-uri OF-TYPE uri WITH-RESOURCE "/ipp/print/$job-id"
	EXPECT job-state OF-TYPE enum WITH-VALUE 4
	EXPECT job-state-reasons WITH-VALUE "job-data-insufficient"
}
{
	NAME "Send-Document"
	OPERATION Send-Document
	GROUP operation-attributes-tag
	ATTR charset attributes-charset utf-8
	ATTR naturalLanguage attributes-natural-language en
	ATTR uri printer-uri $uri
	ATTR integer job-id $job-id
	ATTR boolean last-document true
	ATTR mimeMediaType document-format image/urf
	ATTR name requesting-user-name mobile
	FILE $filename
	STATUS successful-ok
	EXPECT job-id WITH-VALUE $job-id
	EXPECT job-state OF-TYPE enum WITH-VALUE 3,5,9
}
{
	NAME "Get-Printer-Attributes printer-state-reasons"
	OPERATION Get-Printer-Attributes
	GROUP operation-attributes-tag
	ATTR charset attributes-charset utf-8
	ATTR naturalLanguage attributes-natural-language en
	ATTR uri printer-uri $uri
	ATTR name requesting-user-name mobile
	ATTR keyword requested-attributes printer-state-reasons
	STATUS successful-ok
	EXPECT printer-state-reasons
}
{
	NAME "Get-Job-Attributes poll until completed"
	OPERATION Get-Job-Attributes
	DELAY 0.05
	GROUP operation-attributes-tag
	ATTR charset attributes-charset utf-8
	ATTR naturalLanguage attributes-natural-language en
	ATTR uri printer-uri $uri
	ATTR integer job-id $job-id
	ATTR name requesting-user-name mobile
	ATTR keyword requested-attributes job-media-sheets-completed,job-impressions-completed,job-state,job-state-reasons
	STATUS successful-ok
	EXPECT job-state OF-TYPE enum WITH-VALUE 9 REPEAT-NO-MATCH REPEAT-LIMIT 400
}
{
	NAME "Get-Job-Attributes after completion"
	OPERATION Get-Job-Attributes
	GROUP operation-attributes-tag
	ATTR charset attributes-charset utf-8
	ATTR naturalLanguage attributes-natural-language en
	ATTR uri printer-uri $uri
	ATTR integer job-id $job-id
	ATTR name requesting-user-name mobile
	ATTR keyword requested-attributes job-media-sheets-completed,job-impressions-completed,job-state,job-state-reasons
	STATUS successful-ok
	EXPECT job-impressions-completed WITH-VALUE 1
	EXPECT job-state WITH-VALUE 9
	EXPECT job-state-reasons WITH-VALUE "job-completed-successfully"
}
{
	NAME "Print-Job synthetic URF, preset media-col, fidelity"
	OPERATION Print-Job
	GROUP operation-attributes-tag
	ATTR charset attributes-charset utf-8
	ATTR naturalLanguage attributes-natural-language en
	ATTR uri printer-uri $uri
	ATTR name requesting-user-name mobile
	ATTR name job-name "Print-Job label"
	ATTR mimeMediaType document-format image/urf
	ATTR boolean ipp-attribute-fidelity true
	GROUP job-attributes-tag
	ATTR collection media-col { MEMBER collection media-size { MEMBER integer x-dimension 6200 MEMBER integer y-dimension 5000 } MEMBER integer media-left-margin 152 MEMBER integer media-right-margin 152 MEMBER integer media-top-margin 0 MEMBER integer media-bottom-margin 0 }
	FILE $filename
	STATUS successful-ok
	EXPECT job-id OF-TYPE integer WITH-VALUE >0
}
{
	NAME "Get-Job-Attributes Print-Job until completed"
	OPERATION Get-Job-Attributes
	DELAY 0.05
	GROUP operation-attributes-tag
	ATTR charset attributes-charset utf-8
	ATTR naturalLanguage attributes-natural-language en
	ATTR uri printer-uri $uri
	ATTR integer job-id $job-id
	ATTR name requesting-user-name mobile
	ATTR keyword requested-attributes job-state
	STATUS successful-ok
	EXPECT job-state OF-TYPE enum WITH-VALUE 9 REPEAT-NO-MATCH REPEAT-LIMIT 400
}
{
	NAME "Get-Jobs completed"
	OPERATION Get-Jobs
	GROUP operation-attributes-tag
	ATTR charset attributes-charset utf-8
	ATTR naturalLanguage attributes-natural-language en
	ATTR uri printer-uri $uri
	ATTR keyword which-jobs completed
	ATTR keyword requested-attributes job-id,job-state,job-name
	STATUS successful-ok
	EXPECT job-id COUNT 1
	EXPECT job-state WITH-ALL-VALUES 9
}
`

func TestIPPToolIOSReplay(t *testing.T) {
	ipptool := requireIPPTool(t)
	dir := t.TempDir()
	testFile := filepath.Join(dir, "ios-replay.test")
	urfFile := filepath.Join(dir, "label.urf")
	urf := tinyURF(732, 197)
	if err := os.WriteFile(testFile, []byte(iosReplay), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(urfFile, urf, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, scheme := range []string{"ipps", "ipp"} {
		t.Run(scheme, func(t *testing.T) {
			rec := &recorder{}
			slow := func(ctx context.Context, j *Job, doc io.Reader, format string) error {
				// Long enough that the poll sees processing before completed.
				select {
				case <-time.After(200 * time.Millisecond):
				case <-ctx.Done():
					return ctx.Err()
				}
				return rec.process(ctx, j, doc, format)
			}
			ts := startServer(t, testConfig(), slow)
			cmd := exec.Command(ipptool, "-tv", "-T", "30", "-f", urfFile, ts.uri(scheme), testFile)
			out, err := cmd.CombinedOutput()
			if err != nil || bytes.Contains(out, []byte("[FAIL]")) {
				t.Fatalf("ipptool: %v\n%s", err, out)
			}
			if n := strings.Count(string(out), "[PASS]"); n != 17 {
				t.Errorf("%d tests passed, want 17\n%s", n, out)
			}
			rec.mu.Lock()
			defer rec.mu.Unlock()
			if len(rec.docs) != 2 {
				t.Fatalf("processor ran %d jobs", len(rec.docs))
			}
			for i, d := range rec.docs {
				if !bytes.Equal(d, urf) || rec.formats[i] != formatURF {
					t.Errorf("job %d: %d bytes %s", i, len(d), rec.formats[i])
				}
			}
			if mc := rec.jobs[0].Attrs.MediaCol; mc == nil || mc.XDimension != 6200 || mc.YDimension != 1670 || mc.Left != 152 {
				t.Errorf("Create-Job media-col %+v", mc)
			}
			if mc := rec.jobs[1].Attrs.MediaCol; mc == nil || mc.YDimension != 5000 {
				t.Errorf("Print-Job media-col %+v", mc)
			}
			if rec.jobs[0].User != "mobile" || rec.jobs[0].Name != "Labels #26" {
				t.Errorf("job %q by %q", rec.jobs[0].Name, rec.jobs[0].User)
			}
		})
	}
}
