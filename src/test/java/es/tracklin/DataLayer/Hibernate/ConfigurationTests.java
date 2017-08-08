package es.tracklin.DataLayer.Hibernate;

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
    es.tracklin.Configuration.Hibernate hibernateConfiguration;

    @Test
    public void shouldGetAddress() throws Exception {
        assertNotEquals("", hibernateConfiguration.getAddress());
    }

    @Test
    public void shouldGetPort() throws Exception {
        assertNotEquals(0, hibernateConfiguration.getPort());
    }

    @Test
    public void shouldGetUsername() throws Exception {
        assertNotEquals("", hibernateConfiguration.getCredentials().getUsername());
    }
    @Test
    public void shouldGetPassword() throws Exception {
        assertNotEquals("", hibernateConfiguration.getCredentials().getPassword());
    }

    @Test
    public void shouldGetDatabase() throws Exception {
        assertNotEquals("", hibernateConfiguration.getCredentials().getDatabase());
    }
}
