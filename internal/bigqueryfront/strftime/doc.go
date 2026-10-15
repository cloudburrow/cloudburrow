// parser.go is under this license:
//
// MIT License
//
// Copyright (c) 2026 Masaaki Goshima
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

// Package strftime reads a DATE, DATETIME, TIME or TIMESTAMP in a
// C-style format: GoogleSQL's format elements for PARSE_DATE,
// PARSE_DATETIME, PARSE_TIME and PARSE_TIMESTAMP (%Y, %m, %d, %H, ...;
// https://cloud.google.com/bigquery/docs/reference/standard-sql/format-elements#format_elements_date_time),
// for a CSV load's dateFormat, datetimeFormat, timeFormat and
// timestampFormat, which the official Go client's FileConfig says
// "Supports C-style and SQL-style values" (cloudburrow/cloudburrow#1338).
//
// parser.go is the parser of the emulator's engine, googlesqlite, copied
// so a load reads a value as the engine's PARSE_DATE does, with one
// change: a literal character of the format must be the value's, as
// BigQuery's parse requires; the engine skipped it unread.
//
// parser.go is under the MIT License above.
package strftime
