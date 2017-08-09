package es.tracklin.DataLayer.Database;

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
    es.tracklin.Configuration.Database databaseConfiguration;

    @Test
    public void shouldGetAddress() throws Exception {
        assertNotEquals("", databaseConfiguration.getAddress());
    }

    @Test
    public void shouldGetPort() throws Exception {
        assertNotEquals(0, databaseConfiguration.getPort());
    }

    @Test
    public void shouldGetUsername() throws Exception {
        assertNotEquals("", databaseConfiguration.getCredentials().getUsername());
    }
    @Test
    public void shouldGetPassword() throws Exception {
        assertNotEquals("", databaseConfiguration.getCredentials().getPassword());
    }

    @Test
    public void shouldGetDatabase() throws Exception {
        assertNotEquals("", databaseConfiguration.getCredentials().getDatabase());
    }
}
