package main

// Excel（.xlsx）读写。
//
// 不引第三方库：xlsx 本质是一个装着几份 XML 的 zip，
// 这里按 OOXML 最小可用子集手写生成、手写解析。
// 版式（8 列、合计行、冻结首行、列宽、样式）与原版导出文件保持一致，
// 导出文件再用本程序导入能原样回读。

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const (
	xlsxContentType = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"

	// 导入行数上限：超出部分计入 skipped 并置 truncated。
	maxImportRows = 50000
	// 表格列固定顺序（位置固定，不依赖表头文字，与原版一致）。
	colCount = 8
)

var xlsxHeaders = [colCount]string{"账本", "日期", "方向", "对象", "事件", "关系", "金额", "备注"}

var xlsxColWidths = [colCount]int{16, 12, 6, 14, 12, 12, 12, 24}

const xmlHead = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`

// amountText 把金额格式化成不带多余零的十进制文本：600.46 / 800。
func amountText(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// sstBuilder 收集共享字符串并去重，索引即单元格里的 <v>。
type sstBuilder struct {
	list  []string
	index map[string]int
}

func newSST() *sstBuilder {
	return &sstBuilder{index: make(map[string]int)}
}

func (s *sstBuilder) add(v string) int {
	if i, ok := s.index[v]; ok {
		return i
	}
	i := len(s.list)
	s.index[v] = i
	s.list = append(s.list, v)
	return i
}

// colName 把 0 起的列号转成 Excel 列名：0->A, 25->Z, 26->AA。
func colName(i int) string {
	s := ""
	for i >= 0 {
		s = string(rune('A'+i%26)) + s
		i = i/26 - 1
	}
	return s
}

// colIndex 把列名转回 0 起的列号；非法返回 -1。
func colIndex(ref string) int {
	n := 0
	seen := false
	for _, r := range ref {
		if r >= 'A' && r <= 'Z' {
			n = n*26 + int(r-'A'+1)
			seen = true
		} else if r >= 'a' && r <= 'z' {
			n = n*26 + int(r-'a'+1)
			seen = true
		} else {
			break
		}
	}
	if !seen {
		return -1
	}
	return n - 1
}

// esc 处理 XML 文本/属性转义。
func esc(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '"':
			b.WriteString("&quot;")
		case '\'':
			b.WriteString("&apos;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// buildXLSX 生成导出文件。
//
// 版式要点：
//   - 表头加粗浅底（样式 1），金额列数字格式 #,##0.00（样式 2）
//   - 首行冻结，列宽固定
//   - 有记录时末尾追加合计行：金额列为「收-送」，备注列写「收 X / 送 Y」
func buildXLSX(s *State) ([]byte, error) {
	sst := newSST()
	accName := make(map[string]string, len(s.Accounts))
	for _, a := range s.Accounts {
		accName[a.ID] = a.Name
	}

	var rows strings.Builder

	// 表头
	rows.WriteString(`<row r="1">`)
	for i, h := range xlsxHeaders {
		idx := sst.add(h)
		fmt.Fprintf(&rows, `<c r="%s1" s="1" t="s"><v>%d</v></c>`, colName(i), idx)
	}
	rows.WriteString(`</row>`)

	// 数据行
	var in, out float64
	for n, r := range s.Records {
		rowNum := n + 2
		direction := "收"
		if r.Direction == "out" {
			direction = "送"
			out += r.Amount
		} else {
			in += r.Amount
		}
		vals := [colCount]string{accName[r.AccountID], r.Date, direction, r.Name, r.Event, r.Relation, amountText(r.Amount), r.Note}
		rows.WriteString(fmt.Sprintf(`<row r="%d">`, rowNum))
		for c, v := range vals {
			ref := fmt.Sprintf("%s%d", colName(c), rowNum)
			idx := sst.add(v)
			if c == 6 {
				// 金额列写成数值单元格，方便在 Excel 里求和
				fmt.Fprintf(&rows, `<c r="%s" s="2"><v>%s</v></c>`, ref, v)
				continue
			}
			fmt.Fprintf(&rows, `<c r="%s" t="s"><v>%d</v></c>`, ref, idx)
		}
		rows.WriteString(`</row>`)
	}

	// 合计行
	if len(s.Records) > 0 {
		rowNum := len(s.Records) + 2
		total := in - out
		summary := "收 " + amountText(in) + " / 送 " + amountText(out)
		type totalCell struct {
			col int
			val string
			sst int
		}
		cells := []totalCell{
			{0, "合计", 0},
			{1, "", 0},
			{2, "", 0},
			{3, "", 0},
			{4, "", 0},
			{5, "", 0},
			{6, amountText(total), 0},
			{7, summary, 0},
		}
		for i := range cells {
			cells[i].sst = sst.add(cells[i].val)
		}
		rows.WriteString(fmt.Sprintf(`<row r="%d">`, rowNum))
		for _, c := range cells {
			ref := fmt.Sprintf("%s%d", colName(c.col), rowNum)
			if c.col == 6 {
				fmt.Fprintf(&rows, `<c r="%s" s="2" t="s"><v>%d</v></c>`, ref, c.sst)
				continue
			}
			fmt.Fprintf(&rows, `<c r="%s" s="1" t="s"><v>%d</v></c>`, ref, c.sst)
		}
		rows.WriteString(`</row>`)
	}

	var cols strings.Builder
	for i, w := range xlsxColWidths {
		fmt.Fprintf(&cols, `<col min="%d" max="%d" width="%d" customWidth="1"/>`, i+1, i+1, w)
	}

	sheet := xmlHead + `<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">` +
		`<sheetViews><sheetView workbookViewId="0"><pane ySplit="1" topLeftCell="A2" activePane="bottomLeft" state="frozen"/></sheetView></sheetViews>` +
		`<cols>` + cols.String() + `</cols>` +
		`<sheetData>` + rows.String() + `</sheetData></worksheet>`

	var sstXML strings.Builder
	sstXML.WriteString(xmlHead)
	fmt.Fprintf(&sstXML, `<sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" count="%d" uniqueCount="%d">`, len(sst.list), len(sst.list))
	for _, v := range sst.list {
		fmt.Fprintf(&sstXML, `<si><t xml:space="preserve">%s</t></si>`, esc(v))
	}
	sstXML.WriteString(`</sst>`)

	contentTypes := xmlHead + `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
		`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>` +
		`<Default Extension="xml" ContentType="application/xml"/>` +
		`<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>` +
		`<Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>` +
		`<Override PartName="/xl/sharedStrings.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sharedStrings+xml"/>` +
		`<Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/>` +
		`</Types>`

	rels := xmlHead + `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
		`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/>` +
		`</Relationships>`

	workbook := xmlHead + `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">` +
		`<sheets><sheet name="情谊有账" sheetId="1" r:id="rId1"/></sheets></workbook>`

	wbRels := xmlHead + `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
		`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/>` +
		`<Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/sharedStrings" Target="sharedStrings.xml"/>` +
		`<Relationship Id="rId3" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/>` +
		`</Relationships>`

	styles := xmlHead + `<styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">` +
		`<fonts count="2"><font><sz val="11"/><name val="微软雅黑"/></font><font><b/><sz val="11"/><name val="微软雅黑"/></font></fonts>` +
		`<fills count="3"><fill><patternFill patternType="none"/></fill><fill><patternFill patternType="gray125"/></fill><fill><patternFill patternType="solid"><fgColor rgb="FFF4EDE1"/><bgColor indexed="64"/></patternFill></fill></fills>` +
		`<borders count="1"><border><left/><right/><top/><bottom/><diagonal/></border></borders>` +
		`<cellStyleXfs count="1"><xf numFmtId="0" fontId="0" fillId="0" borderId="0"/></cellStyleXfs>` +
		`<cellXfs count="3"><xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0"/><xf numFmtId="0" fontId="1" fillId="2" borderId="0" xfId="0" applyFont="1" applyFill="1"/><xf numFmtId="4" fontId="0" fillId="0" borderId="0" xfId="0" applyNumberFormat="1"/></cellXfs>` +
		`</styleSheet>`

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	files := []struct {
		name string
		body string
	}{
		{"[Content_Types].xml", contentTypes},
		{"_rels/.rels", rels},
		{"xl/workbook.xml", workbook},
		{"xl/_rels/workbook.xml.rels", wbRels},
		{"xl/styles.xml", styles},
		{"xl/sharedStrings.xml", sstXML.String()},
		{"xl/worksheets/sheet1.xml", sheet},
	}
	for _, f := range files {
		w, err := zw.Create(f.name)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write([]byte(f.body)); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ---------- 解析 ----------

type xmlCell struct {
	R  string `xml:"r,attr"`
	T  string `xml:"t,attr"`
	V  string `xml:"v"`
	Is struct {
		T []string `xml:"t"`
	} `xml:"is"`
}

type xmlRow struct {
	Cells []xmlCell `xml:"c"`
}

type xmlSI struct {
	T []string `xml:"t"`
	R []struct {
		T string `xml:"t"`
	} `xml:"r"`
}

type xmlSST struct {
	SI []xmlSI `xml:"si"`
}

// parseXLSX 解析导入文件，返回可合并进账本的记录行。
//
// 规则（逐条实测原版得出）：
//   - 跳过第一行（表头文字不参与判定，列位固定）
//   - A 列去空格后为「合计」的行静默跳过，不计入 skipped
//   - 对象为空、金额无法解析为数字或为负数的行计入 skipped
//   - 其余字段原样返回，由前端/调用方决定如何收敛
//   - 最多 50000 行，多出的计入 skipped 并置 truncated
func parseXLSX(data []byte) (rows []map[string]string, skipped int, truncated bool, err error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, 0, false, err
	}

	var sheetFile, sstFile *zip.File
	for _, f := range zr.File {
		name := strings.ToLower(f.Name)
		switch {
		case name == "xl/sharedstrings.xml":
			sstFile = f
		case name == "xl/worksheets/sheet1.xml":
			sheetFile = f
		case sheetFile == nil && strings.HasPrefix(name, "xl/worksheets/") && strings.HasSuffix(name, ".xml"):
			sheetFile = f
		}
	}
	if sheetFile == nil {
		return nil, 0, false, fmt.Errorf("no worksheet")
	}

	var shared []string
	if sstFile != nil {
		rc, err := sstFile.Open()
		if err != nil {
			return nil, 0, false, err
		}
		var sst xmlSST
		decErr := xml.NewDecoder(rc).Decode(&sst)
		rc.Close()
		if decErr != nil {
			return nil, 0, false, decErr
		}
		for _, si := range sst.SI {
			var b strings.Builder
			for _, t := range si.T {
				b.WriteString(t)
			}
			for _, r := range si.R {
				b.WriteString(r.T)
			}
			shared = append(shared, b.String())
		}
	}

	rc, err := sheetFile.Open()
	if err != nil {
		return nil, 0, false, err
	}
	defer rc.Close()

	rows = []map[string]string{}
	dec := xml.NewDecoder(rc)
	rowSeen := 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			// 表格 XML 尾部残缺时保留已解析的行，不整单判失败
			return rows, skipped, truncated, nil
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "row" {
			continue
		}
		var r xmlRow
		if err := dec.DecodeElement(&r, &se); err != nil {
			return rows, skipped, truncated, nil
		}
		rowSeen++
		if rowSeen == 1 {
			continue // 表头
		}

		var vals [colCount]string
		for _, c := range r.Cells {
			idx := colIndex(c.R)
			if idx < 0 || idx >= colCount {
				continue
			}
			switch c.T {
			case "s":
				n, err := strconv.Atoi(strings.TrimSpace(c.V))
				if err == nil && n >= 0 && n < len(shared) {
					vals[idx] = shared[n]
				}
			case "inlineStr":
				var b strings.Builder
				for _, t := range c.Is.T {
					b.WriteString(t)
				}
				vals[idx] = b.String()
			default:
				vals[idx] = c.V
			}
		}

		if strings.TrimSpace(vals[0]) == "合计" {
			continue
		}
		if strings.TrimSpace(vals[3]) == "" {
			skipped++
			continue
		}
		// 金额留空的行原版放行（由前端按"非法金额"丢弃），
		// 只有填了内容却解析不成数、或填成负数的才在这里计入 skipped。
		if raw := strings.TrimSpace(vals[6]); raw != "" {
			if amt, perr := strconv.ParseFloat(raw, 64); perr != nil || amt < 0 {
				skipped++
				continue
			}
		}
		if len(rows) >= maxImportRows {
			skipped++
			truncated = true
			continue
		}

		rows = append(rows, map[string]string{
			"account":   vals[0],
			"date":      vals[1],
			"direction": vals[2],
			"name":      vals[3],
			"event":     vals[4],
			"relation":  vals[5],
			"amount":    vals[6],
			"note":      vals[7],
		})
	}
	return rows, skipped, truncated, nil
}
