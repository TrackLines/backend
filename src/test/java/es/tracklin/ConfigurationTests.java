/*
 * MIT License
 *
 * Copyright (c) 2017 TrackLin.es
 *
 * Permission is hereby granted, free of charge, to any person obtaining a copy
 * of this software and associated documentation files (the "Software"), to deal
 * in the Software without restriction, including without limitation the rights
 * to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
 * copies of the Software, and to permit persons to whom the Software is
 * furnished to do so, subject to the following conditions:
 *
 * The above copyright notice and this permission notice shall be included in all
 * copies or substantial portions of the Software.
 *
 * THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
 * IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
 * FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
 * AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
 * LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
 * OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
 * SOFTWARE.
 */

package es.tracklin;

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

    @Test
    public void shouldReturnDatabase() throws Exception {
        assertNotEquals("", mongoConfiguration.getDatabase());
    }
}
