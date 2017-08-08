package es.tracklin.DataLayer.ElasticSearch;

import es.tracklin.Backend;
import org.junit.Test;
import org.junit.runner.RunWith;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.boot.test.context.SpringBootTest;
import org.springframework.test.context.junit4.SpringRunner;

import static org.junit.Assert.assertNotEquals;

@RunWith(SpringRunner.class)
@SpringBootTest(classes = Backend.class)
public class ConfigurationTests {
    @Autowired
    es.tracklin.Configuration.ElasticSearch elasticSearchConfiguration;

    @Test
    public void shouldGetAddress() throws Exception {
        assertNotEquals("", elasticSearchConfiguration.getAddress());
    }

    @Test
    public void shouldGetPort() throws Exception {
        assertNotEquals(0, elasticSearchConfiguration.getPort());
    }

    @Test
    public void shouldGetUsername() throws Exception {
        assertNotEquals("", elasticSearchConfiguration.getCredentials().getUsername());
    }

    @Test
    public void shouldGetPassword() throws Exception {
        assertNotEquals("", elasticSearchConfiguration.getCredentials().getPassword());
    }
}
