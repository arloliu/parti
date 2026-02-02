package partition

type partitionCore struct {
	config PartitionConfig

	precomputedSubjects []string
	patternParts        patternParts
}

func newPartitionCore(config PartitionConfig) (*partitionCore, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	parts, err := parsePattern(config.SubjectPattern)
	if err != nil {
		return nil, err
	}

	// Publishers must produce concrete subjects (no wildcards).
	sample := parts.buildSubject("key", 0)
	if err := validateSubjectTokens(sample, false); err != nil {
		return nil, err
	}

	core := &partitionCore{
		config:       config,
		patternParts: parts,
	}

	if !parts.hasKey {
		core.precomputedSubjects = make([]string, config.NumPartitions)
		for i := range config.NumPartitions {
			core.precomputedSubjects[i] = parts.buildSubjectNoKey(i)
		}
	}

	return core, nil
}

func (c *partitionCore) getPartition(key string) int {
	return computePartition(key, c.config.NumPartitions, c.config.HashSeed)
}

func (c *partitionCore) getSubject(key string) string {
	partition := c.getPartition(key)
	if partition < 0 {
		return ""
	}

	return c.subjectForKey(key, partition)
}

func (c *partitionCore) getSubjectForPartition(partition int) (string, error) {
	if partition < 0 || partition >= c.config.NumPartitions {
		return "", ErrPartitionOutOfRange
	}

	return c.patternParts.buildFilterSubject(partition), nil
}

func (c *partitionCore) numPartitions() int {
	return c.config.NumPartitions
}

func (c *partitionCore) subjectForKey(key string, partition int) string {
	if partition < 0 {
		return ""
	}
	if c.precomputedSubjects != nil {
		return c.precomputedSubjects[partition]
	}

	return c.patternParts.buildSubject(key, partition)
}
