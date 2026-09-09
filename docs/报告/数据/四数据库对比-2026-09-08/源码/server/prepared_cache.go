package server

import "fmt"

const defaultMaxPreparedStatements = 128
const defaultMaxPreparedBytes = 1 << 20

type preparedCache struct {
	statements map[uint32]*preparedStatement
	nextID     uint32
	bytes      int
	maxCount   int
	maxBytes   int
}

func newPreparedCache(maxCount, maxBytes int) *preparedCache {
	if maxCount <= 0 {
		maxCount = defaultMaxPreparedStatements
	}
	if maxBytes <= 0 {
		maxBytes = defaultMaxPreparedBytes
	}
	return &preparedCache{statements: make(map[uint32]*preparedStatement), nextID: 1, maxCount: maxCount, maxBytes: maxBytes}
}
func (c *preparedCache) add(query string) (uint32, *preparedStatement, uint16, error) {
	if len(c.statements) >= c.maxCount || len(query) > c.maxBytes-c.bytes {
		return 0, nil, 1461, fmt.Errorf("prepared statement limit reached (count %d, SQL bytes %d); close unused statements", c.maxCount, c.maxBytes)
	}
	parameters := countPlaceholders(query)
	if parameters > 65535 {
		return 0, nil, 1390, fmt.Errorf("prepared statement contains too many placeholders")
	}
	// Include the retained parameter type vector in the budget, before execute.
	cost := len(query) + parameters*2
	if cost > c.maxBytes-c.bytes {
		return 0, nil, 1461, fmt.Errorf("prepared statement memory budget exceeded")
	}
	id := c.nextID
	for {
		if _, exists := c.statements[id]; !exists && id != 0 {
			break
		}
		id++
	}
	c.nextID = id + 1
	statement := &preparedStatement{query: query, parameterCount: parameters}
	c.statements[id] = statement
	c.bytes += cost
	return id, statement, 0, nil
}
func (c *preparedCache) remove(id uint32) {
	if statement := c.statements[id]; statement != nil {
		c.bytes -= len(statement.query) + statement.parameterCount*2
		delete(c.statements, id)
	}
}
