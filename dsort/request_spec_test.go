/*
 * Copyright (c) 2018, NVIDIA CORPORATION. All rights reserved.
 *
 */

// Package dsort provides APIs for distributed archive file shuffling.
package dsort

import (
	"github.com/NVIDIA/aistore/cmn"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("RequestSpec", func() {
	Context("requests specs which should pass", func() {
		It("should parse minimal spec", func() {
			rs := RequestSpec{
				Bucket:          "test",
				Extension:       extTar,
				IntputFormat:    "prefix-{0010..0111..2}-suffix",
				OutputFormat:    "prefix-{10..111}-suffix",
				OutputShardSize: 100000,
				MaxMemUsage:     "80%",
				Algorithm:       SortAlgorithm{Kind: SortKindNone},
			}
			parsed, err := rs.Parse()
			Expect(err).ShouldNot(HaveOccurred())

			Expect(parsed.Bucket).To(Equal("test"))
			Expect(parsed.OutputBucket).To(Equal("test"))
			Expect(parsed.BckProvider).To(Equal(cmn.LocalBs))
			Expect(parsed.OutputBckProvider).To(Equal(cmn.LocalBs))
			Expect(parsed.Extension).To(Equal(extTar))

			Expect(parsed.InputFormat.Template).To(Equal(cmn.ParsedTemplate{
				Prefix: "prefix-",
				Ranges: []cmn.TemplateRange{{
					Start:      10,
					End:        111,
					Step:       2,
					DigitCount: 4,
					Gap:        "-suffix",
				}},
			}))

			Expect(parsed.OutputFormat.Template).To(Equal(cmn.ParsedTemplate{
				Prefix: "prefix-",
				Ranges: []cmn.TemplateRange{{
					Start:      10,
					End:        111,
					Step:       1,
					DigitCount: 2,
					Gap:        "-suffix",
				}},
			}))

			Expect(parsed.OutputShardSize).To(BeEquivalentTo(100000))

			Expect(parsed.MaxMemUsage.Type).To(Equal(memPercent))
			Expect(parsed.MaxMemUsage.Value).To(BeEquivalentTo(80))
		})

		It("should set buckets correctly", func() {
			rs := RequestSpec{
				Bucket:            "test",
				BckProvider:       cmn.CloudBs,
				OutputBucket:      "testing",
				OutputBckProvider: cmn.CloudBs,
				Extension:         extTar,
				IntputFormat:      "prefix-{0010..0111..2}-suffix",
				OutputFormat:      "prefix-{10..111}-suffix",
				OutputShardSize:   100000,
				MaxMemUsage:       "80%",
				Algorithm:         SortAlgorithm{Kind: SortKindNone},
			}
			parsed, err := rs.Parse()
			Expect(err).ShouldNot(HaveOccurred())

			Expect(parsed.Bucket).To(Equal("test"))
			Expect(parsed.OutputBucket).To(Equal("testing"))
			Expect(parsed.BckProvider).To(Equal(cmn.CloudBs))
			Expect(parsed.OutputBckProvider).To(Equal(cmn.CloudBs))
		})

		It("should parse spec with mem usage as bytes", func() {
			rs := RequestSpec{
				Bucket:          "test",
				Extension:       extTar,
				IntputFormat:    "prefix-{0010..0111}-suffix",
				OutputFormat:    "prefix-{0010..0111}-suffix",
				OutputShardSize: 100000,
				MaxMemUsage:     "80 GB",
				Algorithm:       SortAlgorithm{Kind: SortKindNone},
			}
			parsed, err := rs.Parse()
			Expect(err).ShouldNot(HaveOccurred())

			Expect(parsed.MaxMemUsage.Type).To(Equal(memNumber))
			Expect(parsed.MaxMemUsage.Value).To(BeEquivalentTo(80 * 1024 * 1024 * 1024))
		})

		It("should parse spec with .tgz extension", func() {
			rs := RequestSpec{
				Bucket:          "test",
				Extension:       extTgz,
				IntputFormat:    "prefix-{0010..0111}-suffix",
				OutputFormat:    "prefix-{0010..0111}-suffix",
				OutputShardSize: 100000,
				Algorithm:       SortAlgorithm{Kind: SortKindNone},
			}
			parsed, err := rs.Parse()
			Expect(err).ShouldNot(HaveOccurred())

			Expect(parsed.Extension).To(Equal(extTgz))
		})

		It("should parse spec with .tar.gz extension", func() {
			rs := RequestSpec{
				Bucket:          "test",
				Extension:       extTarTgz,
				IntputFormat:    "prefix-{0010..0111}-suffix",
				OutputFormat:    "prefix-{0010..0111}-suffix",
				OutputShardSize: 100000,
				Algorithm:       SortAlgorithm{Kind: SortKindNone},
			}
			parsed, err := rs.Parse()
			Expect(err).ShouldNot(HaveOccurred())

			Expect(parsed.Extension).To(Equal(extTarTgz))
		})

		It("should parse spec with .tar.gz extension", func() {
			rs := RequestSpec{
				Bucket:          "test",
				Extension:       extZip,
				IntputFormat:    "prefix-{0010..0111}-suffix",
				OutputFormat:    "prefix-{0010..0111}-suffix",
				OutputShardSize: 100000,
				Algorithm:       SortAlgorithm{Kind: SortKindNone},
			}
			parsed, err := rs.Parse()
			Expect(err).ShouldNot(HaveOccurred())

			Expect(parsed.Extension).To(Equal(extZip))
		})

		It("should parse spec with @ syntax", func() {
			rs := RequestSpec{
				Bucket:          "test",
				Extension:       extTgz,
				IntputFormat:    "prefix@0111-suffix",
				OutputFormat:    "prefix-@000111-suffix",
				OutputShardSize: 100000,
				Algorithm:       SortAlgorithm{Kind: SortKindNone},
			}
			parsed, err := rs.Parse()
			Expect(err).ShouldNot(HaveOccurred())

			Expect(parsed.InputFormat.Type).To(Equal(templAt))
			Expect(parsed.InputFormat.Template).To(Equal(cmn.ParsedTemplate{
				Prefix: "prefix",
				Ranges: []cmn.TemplateRange{{
					Start:      0,
					End:        111,
					Step:       1,
					DigitCount: 4,
					Gap:        "-suffix",
				}},
			}))

			Expect(parsed.OutputFormat.Template).To(Equal(cmn.ParsedTemplate{
				Prefix: "prefix-",
				Ranges: []cmn.TemplateRange{{
					Start:      0,
					End:        111,
					Step:       1,
					DigitCount: 6,
					Gap:        "-suffix",
				}},
			}))

		})

		It("should parse spec and set default conc limits", func() {
			rs := RequestSpec{
				Bucket:           "test",
				Extension:        extTar,
				IntputFormat:     "prefix-{0010..0111}-suffix",
				OutputFormat:     "prefix-{0010..0111}-suffix",
				OutputShardSize:  10,
				CreateConcLimit:  0,
				ExtractConcLimit: 0,
				Algorithm:        SortAlgorithm{Kind: SortKindNone},
			}
			parsed, err := rs.Parse()
			Expect(err).ShouldNot(HaveOccurred())

			Expect(parsed.CreateConcLimit).To(Equal(defaultConcLimit))
			Expect(parsed.ExtractConcLimit).To(Equal(defaultConcLimit))
		})
	})

	Context("request specs which shall NOT pass", func() {
		It("should fail due to missing bucket property", func() {
			rs := RequestSpec{
				Extension:       ".txt",
				OutputShardSize: 100000,
				Algorithm:       SortAlgorithm{Kind: SortKindNone},
			}
			_, err := rs.Parse()
			Expect(err).Should(HaveOccurred())
			Expect(err).To(Equal(errMissingBucket))
		})

		It("should fail due to start after end in input format", func() {
			rs := RequestSpec{
				Bucket:          "test",
				Extension:       extTar,
				OutputShardSize: 100000,
				IntputFormat:    "prefix-{0112..0111}-suffix",
				OutputFormat:    "prefix-{0010..0111}-suffix",
				Algorithm:       SortAlgorithm{Kind: SortKindNone},
			}
			_, err := rs.Parse()
			Expect(err).Should(HaveOccurred())
			Expect(err).To(Equal(errInvalidInputFormat))
		})

		It("should fail due to start after end in output format", func() {
			rs := RequestSpec{
				Bucket:          "test",
				Extension:       extTar,
				OutputShardSize: 100000,
				IntputFormat:    "prefix-{0010..0111}-suffix",
				OutputFormat:    "prefix-{0112..0111}-suffix",
				Algorithm:       SortAlgorithm{Kind: SortKindNone},
			}
			_, err := rs.Parse()
			Expect(err).Should(HaveOccurred())
			Expect(err).To(Equal(errInvalidOutputFormat))
		})

		It("should fail due invalid parentheses", func() {
			rs := RequestSpec{
				Bucket:          "test",
				Extension:       extTar,
				OutputShardSize: 100000,
				IntputFormat:    "prefix-}{0001..0111}-suffix",
				OutputFormat:    "prefix-}{0010..0111}-suffix",
				Algorithm:       SortAlgorithm{Kind: SortKindNone},
			}
			_, err := rs.Parse()
			Expect(err).Should(HaveOccurred())
			Expect(err).To(Equal(errInvalidInputFormat))
		})

		It("should fail due to invalid extension", func() {
			rs := RequestSpec{
				Bucket:          "test",
				Extension:       ".jpg",
				IntputFormat:    "prefix-{0010..0111}-suffix",
				OutputFormat:    "prefix-{0010..0111}-suffix",
				OutputShardSize: 100000,
				Algorithm:       SortAlgorithm{Kind: SortKindNone},
			}
			_, err := rs.Parse()
			Expect(err).Should(HaveOccurred())
			Expect(err).To(Equal(errInvalidExtension))
		})

		It("should fail due to invalid mem usage specification", func() {
			rs := RequestSpec{
				Bucket:          "test",
				Extension:       extTar,
				IntputFormat:    "prefix-{0010..0111}-suffix",
				OutputFormat:    "prefix-{0010..0111}-suffix",
				OutputShardSize: 100000,
				MaxMemUsage:     "80",
				Algorithm:       SortAlgorithm{Kind: SortKindNone},
			}
			_, err := rs.Parse()
			Expect(err).Should(HaveOccurred())
			Expect(err).To(Equal(errInvalidMemUsage))
		})

		It("should fail due to invalid mem usage percent specified", func() {
			rs := RequestSpec{
				Bucket:          "test",
				Extension:       extTar,
				IntputFormat:    "prefix-{0010..0111}-suffix",
				OutputFormat:    "prefix-{0010..0111}-suffix",
				OutputShardSize: 100000,
				MaxMemUsage:     "120%",
				Algorithm:       SortAlgorithm{Kind: SortKindNone},
			}
			_, err := rs.Parse()
			Expect(err).Should(HaveOccurred())
			Expect(err).To(Equal(errInvalidMaxMemPercent))
		})

		It("should fail due to invalid mem usage bytes specified", func() {
			rs := RequestSpec{
				Bucket:          "test",
				Extension:       extTar,
				IntputFormat:    "prefix-{0010..0111}-suffix",
				OutputFormat:    "prefix-{0010..0111}-suffix",
				OutputShardSize: 100000,
				MaxMemUsage:     "-1 GB",
				Algorithm:       SortAlgorithm{Kind: SortKindNone},
			}
			_, err := rs.Parse()
			Expect(err).Should(HaveOccurred())
			Expect(err).To(Equal(errInvalidMemUsage))
		})

		It("should fail due to invalid extract concurrency specified", func() {
			rs := RequestSpec{
				Bucket:           "test",
				Extension:        extTar,
				IntputFormat:     "prefix-{0010..0111}-suffix",
				OutputFormat:     "prefix-{0010..0111}-suffix",
				OutputShardSize:  100000,
				ExtractConcLimit: -1,
				Algorithm:        SortAlgorithm{Kind: SortKindNone},
			}
			_, err := rs.Parse()
			Expect(err).Should(HaveOccurred())
			Expect(err).To(Equal(errNegativeConcurrencyLimit))
		})

		It("should fail due to invalid create concurrency specified", func() {
			rs := RequestSpec{
				Bucket:          "test",
				Extension:       extTar,
				IntputFormat:    "prefix-{0010..0111}-suffix",
				OutputFormat:    "prefix-{0010..0111}-suffix",
				OutputShardSize: 100000,
				CreateConcLimit: -1,
				Algorithm:       SortAlgorithm{Kind: SortKindNone},
			}
			_, err := rs.Parse()
			Expect(err).Should(HaveOccurred())
			Expect(err).To(Equal(errNegativeConcurrencyLimit))
		})
	})
})
