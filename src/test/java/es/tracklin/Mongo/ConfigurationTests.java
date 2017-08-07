package es.tracklin.Mongo;

import es.tracklin.Backend;
import es.tracklin.Configuration.MongoConfiguration;
import org.junit.Test;
import org.junit.runner.RunWith;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.boot.test.context.SpringBootTest;
import org.springframework.test.context.TestPropertySource;
import org.springframework.test.context.junit4.SpringRunner;
import static org.junit.Assert.assertNotEquals;

@RunWith(SpringRunner.class)
@SpringBootTest(classes = Backend.class)
public class ConfigurationTests {
    @Autowired
    MongoConfiguration mongoConfiguration;

    @Test
    public void shouldReturnAddress() throws Exception {
        assertNotEquals("", mongoConfiguration.getAddress());
    }

    @Test
    public void shouldReturnPort() throws Exception {
        assertNotEquals(0, mongoConfiguration.getPort());
    }

    @Test
    public void shouldReturnUsername() throws Exception {
        MongoConfiguration.Credentials credentials = mongoConfiguration.getCredentials();

        assertNotEquals("", credentials.getUsername());
    }

    @Test
    public void shouldReturnPassword() throws Exception {
        MongoConfiguration.Credentials credentials = mongoConfiguration.getCredentials();

        assertNotEquals("", credentials.getPassword());
    }
}
